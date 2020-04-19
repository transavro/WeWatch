package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/golang/protobuf/ptypes/timestamp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	pb "WeWatch/proto"
	"github.com/golang/protobuf/ptypes"
	"google.golang.org/grpc"
)

const tokenHeader = "x-chat-token"

// prime struct
type server struct {
	Host, Password string

	BroadCast chan pb.StreamResponse

	ClientNames   map[string]string
	ClientStreams map[string]chan pb.StreamResponse

	Rooms map[int32][]string

	namesMtx, streamMtx, roomMtx sync.RWMutex
}

// to create new Server obj
func Server(host, pass string) *server {
	return &server{
		Host:     host,
		Password: pass,

		BroadCast: make(chan pb.StreamResponse, 1000),

		ClientNames:   make(map[string]string),
		ClientStreams: make(map[string]chan pb.StreamResponse),

		Rooms: make(map[int32][]string),
	}
}

func (s *server) Run(ctx context.Context) (err error) {

	ctx, cancle := context.WithCancel(ctx)
	defer cancle()

	ServerLogf(time.Now(), "starting on %s with password %q", s.Host, s.Password)

	srv := grpc.NewServer()
	pb.RegisterWeWatchServer(srv, s)

	l, err := net.Listen("tcp", s.Host)

	if err != nil {
		return errors.New(err.Error() + " server unable to bind on provided host")
	}

	go s.primeBroadcast()

	go func() {
		_ = srv.Serve(l)
		cancle()
	}()

	<-ctx.Done()

	s.BroadCast <- pb.StreamResponse{
		Timestamp: ptypes.TimestampNow(),
		Event:     &pb.StreamResponse_ServerShutdown{ServerShutdown: &pb.Shutdown{}},
	}

	close(s.BroadCast)
	ServerLogf(time.Now(), "shutting down")

	srv.GracefulStop()
	return nil
}

func (s *server) Login(_ context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {

	switch {
	// checking server password
	case req.Password != s.Password:
		return nil, status.Error(codes.Unauthenticated, "Server Password is incorrect")
	// check if the name id empty or not
	case req.Name == "":
		return nil, status.Error(codes.InvalidArgument, "Client Name is required")
	}

	// generate token
	tkn := s.genToken()
	s.setName(tkn, req.Name)

	ServerLogf(time.Now(), "%s (%s) has logged in", tkn, req.Name)

	// sending it to a boradcast channel
	s.BroadCast <- pb.StreamResponse{
		Timestamp: ptypes.TimestampNow(),
		Event:     &pb.StreamResponse_ClientLogin{ClientLogin: &pb.Login{Name: req.Name}},
	}

	return &pb.LoginResponse{Token: tkn}, nil
}

func (s *server) Logout(_ context.Context, req *pb.LogoutRequest) (*pb.LogoutResponse, error) {

	//deleting the user
	if name, ok := s.delName(req.Token); !ok {
		return nil, status.Error(codes.NotFound, "token not found")
	} else {

		// removing token from every group
		s.exitRoom(req.Token)

		// if deleted successfully  log it
		ServerLogf(time.Now(), "%s (%s) has logged out", req.Token, name)

		// sending broadcast of the logout event to the channel
		s.BroadCast <- pb.StreamResponse{
			Timestamp: ptypes.TimestampNow(),
			Event:     &pb.StreamResponse_ClientLogout{ClientLogout: &pb.Logout{Name: name}},
		}

		return new(pb.LogoutResponse), nil
	}
}

func (s *server) MakeRoom(_ context.Context, req *pb.MakeRoomRequest) (*pb.MakeRoomResponse, error) {
	// validate token
	if ok := s.isTknValid(req.Token); !ok {
		return nil, status.Error(codes.NotFound, "token not found")
	}

	// checkif the user is present in some room if yes remove him from that room
	s.exitRoom(req.Token)

	// making a new group
	roomId := s.MakeRoomId()
	DebugLogf("Made an new Group %d ", roomId)
	s.Rooms[roomId] = []string{}

	// adding user in this room.
	if r := s.addInRoom(req.Token, roomId); r < 0 {
		return nil, status.Error(codes.NotFound, "Room Id not found")
	} else if r == 0 {
		return &pb.MakeRoomResponse{RoomId: roomId}, errors.New("User already Present in the group.")
	} else {
		return &pb.MakeRoomResponse{RoomId: roomId}, nil
	}
}

func (s *server) JoinRoom(_ context.Context, req *pb.JoinRoomRequest) (*pb.JoinRoomResponse, error) {
	// validate the token
	if ok := s.isTknValid(req.Token); !ok {
		return nil, status.Error(codes.NotFound, "token not found")
	}

	//adding into group
	if r := s.joinInRoom(req.Token, req.RoomId); r < 0 {
		return &pb.JoinRoomResponse{JointState: pb.JoinState_UNKNOWN}, nil
	} else if r == 0 {
		return &pb.JoinRoomResponse{JointState: pb.JoinState_ALREADY}, nil
	} else {
		return &pb.JoinRoomResponse{JointState: pb.JoinState_ADDED}, nil
	}
}


func (s *server) Stream(srv pb.WeWatch_StreamServer) error {

	// extracting the token from the stream context
	token, ok := s.extractToken(srv.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "missing token header")
	}

	name, ok := s.getName(token)

	if !ok {
		return status.Error(codes.Unauthenticated, "invalid token")
	}

	go s.sendBroadCasts(srv, token, name)

	for {
		req, err := srv.Recv()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		s.BroadCast <- pb.StreamResponse{
			Timestamp: ptypes.TimestampNow(),
			Event:     &pb.StreamResponse_ClientMessage{ClientMessage: &pb.Message{Name: name, Message: req.Message}},
		}
	}

	<-srv.Context().Done()
	return srv.Context().Err()
}


func (s *server) sendBroadCasts(srv pb.WeWatch_StreamServer, tkn, name string) {

	DebugLogf("sendBroadCasts %s %s", name, tkn)
	// opening the stream
	stream := s.openStream(tkn, name)
	//closing the stream
	defer s.closeStream(tkn, name)

	for {
		select {
		case <-srv.Context().Done():
			return
		case res := <-stream:

			if s, ok := status.FromError(srv.Send(&res)); ok {
				switch s.Code() {
				case codes.OK:
					DebugLogf("go somthing in stream broadcasting.. %s %d ", name, tkn)
					// no operation
				case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded:
					DebugLogf("client (%s) with token %s terminated connection", name, tkn)
					return
				default:
					ClientLogf(time.Now(), "failed to send to client %s of token %s : %v", name, tkn, s.Err())
					return
				}
			}
		}
	}
}

/*
 here reading the message from channel broadcasts
once you get the msg, run the whole map of saved channels and send them
one by one.
*/

//func (s *server) broadcast() {
//	ServerLogf(time.Now(), "looping the broadcast channel.")
//	for res := range s.BroadCast {
//		ServerLogf(time.Now(), "looping br...")
//		s.streamMtx.RLock()
//		for _, stream := range s.ClientStreams {
//			select {
//			case stream <- res:
//				ServerLogf(time.Now(), "sending to saved stream map")
//			default:
//				ServerLogf(time.Now(), "client stream full, dropping message")
//			}
//		}
//		s.streamMtx.RUnlock()
//	}
//}

func (s *server) primeBroadcast() {
	// reading from channel
	var roomId int32
	for res := range s.BroadCast {
		if res.GetClientLogin() != nil {
			roomId = s.getRoomIdByName(res.GetClientLogin().GetName())
		} else if res.GetClientLogout() != nil {
			roomId = s.getRoomIdByName(res.GetClientLogout().GetName())
		} else if res.GetClientMessage() != nil {
			roomId = s.getRoomIdByName(res.GetClientMessage().GetName())
		} else {
			roomId = 0
		}
		DebugLogf("Chan msg %s  ****%d****", res.String(), roomId)
		if roomId == 0 {
			for _, stream := range s.ClientStreams {
				stream <- res
			}
		} else {
			for _, member := range s.Rooms[roomId] {
				if name, _ := s.getName(member); name == res.GetClientMessage().GetName() {
					continue
				}
				s.ClientStreams[member] <- res
			}
		}
	}
}

func (s *server) openStream(tkn, name string) (stream chan pb.StreamResponse) {
	stream = make(chan pb.StreamResponse, 1000)

	s.streamMtx.Lock()
	s.ClientStreams[tkn] = stream
	s.streamMtx.Unlock()

	DebugLogf("opened stream for client %s of token %s", name, tkn)
	return
}

func (s *server) closeStream(tkn, name string) {
	s.streamMtx.Lock()
	if stream, ok := s.ClientStreams[tkn]; ok {
		delete(s.ClientStreams, tkn)
		close(stream)
	}
	DebugLogf("closed stream for client %s of token %s", name, tkn)
	s.streamMtx.Unlock()
}

//TODO utility functions

// generating token
func (s *server) genToken() string {
	tkn := make([]byte, 4)
	rand.Read(tkn)
	return fmt.Sprintf("%x", tkn)
}

// settingName
func (s *server) setName(tkn, name string) {
	s.namesMtx.Lock()
	s.ClientNames[tkn] = name
	s.namesMtx.Unlock()
}

// getName
func (s *server) getName(tkn string) (name string, ok bool) {
	s.namesMtx.RLock()
	name, ok = s.ClientNames[tkn]
	s.namesMtx.RUnlock()
	return
}

//deletename
func (s *server) delName(tkn string) (name string, ok bool) {
	name, ok = s.getName(tkn)
	if ok {
		s.namesMtx.Lock()
		delete(s.ClientNames, tkn)
		s.namesMtx.Unlock()
	}
	return
}

//extract Token
func (s *server) extractToken(ctx context.Context) (tkn string, ok bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md[tokenHeader]) == 0 {
		return "", false
	}
	return md[tokenHeader][0], true
}

// is Token valid ?
func (s *server) isTknValid(tkn string) (ok bool) {
	s.namesMtx.RLock()
	_, ok = s.ClientNames[tkn]
	s.namesMtx.RUnlock()
	return ok
}

// making new group id
func (s *server) MakeRoomId() int32 {
	s.roomMtx.Lock()
	defer s.roomMtx.Unlock()
	code := 3000 + rand.Int31n(10000-3000)
	if _, ok := s.Rooms[code]; ok {
		return s.MakeRoomId()
	}
	return code
}

// get Token from name
func (s *server) getRoomIdByName(name string) int32 {
	// fetching token
	var tkn string
	s.namesMtx.RLock()
	for k, v := range s.ClientNames {
		if name == v {
			tkn = k
			break
		}
	}
	s.namesMtx.RUnlock()
	if len(tkn) == 0 {
		return 0
	}

	// fetching groups associated with this token
	s.roomMtx.RLock()
	defer s.roomMtx.RUnlock()
	for id, members := range s.Rooms {
		for _, member := range members {
			if member == tkn {
				return id
			}
		}
	}
	return 0
}

// adding member in the room == > 1 = added , 0 = already present , -1 = room id not found
func (s *server) addInRoom(tkn string, roomId int32) int32 {
	s.roomMtx.Lock()
	defer s.roomMtx.Unlock()

	// first check if the room exits or not
	if members, exits := s.Rooms[roomId]; !exits {
		s.Rooms[roomId] = []string{tkn}
		return 1
	} else {
		for _, member := range members {
			if member == tkn {
				return 0
			}
		}
		members = append(members, tkn)
		s.Rooms[roomId] = members
		return 1
	}
}

func (s *server) joinInRoom(tkn string, roomId int32) int32 {
	if members, exits := s.Rooms[roomId]; !exits {
		return -1
	} else {
		for _, member := range members {
			if member == tkn {
				return 0
			}
		}
		members = append(members, tkn)
		s.Rooms[roomId] = members
		return 1
	}
}

func (s *server) exitRoom(tkn string) {
	s.roomMtx.Lock()
	defer s.roomMtx.Unlock()
	for roomId, members := range s.Rooms {
		if len(members) == 1 && members[0] == tkn {
			delete(s.Rooms, roomId)
		}else {
			for i, member := range members {
				if member == tkn {
					members = append(members[:i], members[i+1:]...)
				}
			}
			s.Rooms[roomId] = members
		}
	}
}

// adding member in the room == > 1 = removed , 0 = removed already , -1 = room id not found
func (s *server) leaveRoom(tkn string, roomId int32) int32 {
	s.roomMtx.Lock()
	defer s.roomMtx.Unlock()
	members, ok := s.Rooms[roomId]
	var r int32
	r = 0
	if !ok {
		r = -1
	}
	for i, member := range members {
		if member == tkn {
			members = append(members[:i], members[i+1:]...)
			r = 1
			break
		}
	}

	// if last user removed deleting room
	if r != -1 && len(members) == 0 {
		delete(s.Rooms, roomId)
	}
	return r
}

func tsToTime(ts *timestamp.Timestamp) time.Time {
	t, err := ptypes.Timestamp(ts)
	if err != nil {
		return time.Now()
	}
	return t.In(time.Local)
}
