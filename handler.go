package main

import (
	pb "WeWatch/proto"
	"context"
	"errors"
	"fmt"
	"github.com/golang/protobuf/ptypes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"io"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"
)

const tokenHeader = "x-chat-token"

type prime struct {
	nameMtx, roomMtx, streamMtx sync.RWMutex

	NameMap   map[string]string
	StreamMap map[string]chan pb.StreamResponse
	RoomMap   map[int32][]string

	BroadCast chan pb.StreamResponse
}

func Prime() *prime {
	return &prime{
		NameMap:   make(map[string]string),
		StreamMap: make(map[string]chan pb.StreamResponse),
		RoomMap:   make(map[int32][]string),
		BroadCast: make(chan pb.StreamResponse, 1000),
	}
}

func (s *prime) Run(ctx context.Context) (err error) {

	ctx, cancle := context.WithCancel(ctx)
	defer cancle()
	srv := grpc.NewServer()
	pb.RegisterWeWatchServer(srv, s)

	l, err := net.Listen("tcp", ":5000")
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

func (s *prime) primeBroadcast() {
	// reading from channel
	var (
		roomId int32
		name   string
		temp   string
	)
	for res := range s.BroadCast {
		if res.GetClientLogin() != nil {
			name = res.GetClientLogin().GetName()
			roomId = s.getRoomIdByName(name)
		} else if res.GetClientLogout() != nil {
			name = res.GetClientLogout().GetName()
			roomId = s.getRoomIdByName(name)
		} else if res.GetClientMessage() != nil {
			name = res.GetClientMessage().GetName()
			roomId = s.getRoomIdByName(name)
		} else {
			roomId = 0
		}
		if roomId == 0 {
			for _, stream := range s.StreamMap {
				stream <- res
			}
		} else {
			s.roomMtx.RLock()
			for _, member := range s.RoomMap[roomId] {
				s.nameMtx.RLock()
				temp = s.NameMap[member]
				s.nameMtx.RUnlock()
				if temp == name {
					continue
				}
				s.StreamMap[member] <- res
			}
			s.roomMtx.RUnlock()
		}
	}
}

func (s *prime) getRoomIdByName(name string) int32 {
	// fetching token
	var tkn string
	s.nameMtx.RLock()
	for k, v := range s.NameMap {
		if name == v {
			tkn = k
			break
		}
	}
	s.nameMtx.RUnlock()

	if tkn == "" {
		return 0
	}

	// fetching groups associated with this token
	s.roomMtx.RLock()
	var roomId int32
	for id, members := range s.RoomMap {
		for _, member := range members {
			if member == tkn {
				roomId = id
				break
			}
		}
	}
	s.roomMtx.RUnlock()
	return roomId
}

func (s *prime) Login(_ context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	log.Println("LOGIN HIT...")
	// validating the password and name
	if req.Password != "cloudwalker" {
		return nil, status.Error(codes.Unauthenticated, "Server Password is incorrect")
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "Client Name is required")
	}

	log.Println("NAME PASS VALID...")
	tkn := make([]byte, 4)
	rand.Read(tkn)
	token := fmt.Sprintf("%x", tkn)
	s.nameMtx.Lock()
	s.NameMap[token] = req.Name
	s.nameMtx.Unlock()
	log.Println("TOKEN CREATED ", token)
	s.BroadCast <- pb.StreamResponse{
		Timestamp: ptypes.TimestampNow(),
		Event:     &pb.StreamResponse_ClientLogin{ClientLogin: &pb.Login{Name: req.Name}},
	}

	return &pb.LoginResponse{Token: token}, nil
}

func (s *prime) Logout(_ context.Context, req *pb.LogoutRequest) (*pb.LogoutResponse, error) {
	log.Println("LOGOUT HIT..")
	var ok bool

	// check if the token is present
	s.nameMtx.RLock()
	name, ok := s.NameMap[req.Token]
	s.nameMtx.RUnlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "token not found")
	}
	log.Println("TOKEN VALIDATED...", req.Token)
	// deleting the token
	s.nameMtx.Lock()
	delete(s.NameMap, req.Token)
	s.nameMtx.Unlock()
	log.Println("TOKEN REMOVED FROM NAME MAP ", req.Token)

	// removing the user from every room
	s.roomMtx.Lock()
	for k, v := range s.RoomMap {
		var temp []string
		for _, member := range v {
			if member == req.Token {
				continue
			}
			temp = append(temp, member)
		}
		if len(temp) == 0 {
			delete(s.RoomMap, k)
		}else {
			s.RoomMap[k] = temp
		}
	}
	s.roomMtx.Unlock()
	log.Println("TOKEN REMOVED FROM EVERY ROOM ", s.RoomMap)

	s.BroadCast <- pb.StreamResponse{
		Timestamp: ptypes.TimestampNow(),
		Event:     &pb.StreamResponse_ClientLogout{ClientLogout: &pb.Logout{Name: name}},
	}

	return new(pb.LogoutResponse), nil
}

func (s *prime) MakeRoom(_ context.Context, req *pb.MakeRoomRequest) (*pb.MakeRoomResponse, error) {
	log.Println("MAKE ROOM HIT....")
	// check if the token is valid or not
	var (
		ok     bool
		roomId int32
	)
	s.nameMtx.RLock()
	_, ok = s.NameMap[req.Token]
	s.nameMtx.RUnlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "token not found")
	}
	log.Println("TOKEN VALIDATED..")

	//Making roomID
	for {
		roomId = 3000 + rand.Int31n(10000-3000)
		s.roomMtx.RLock()
		_, ok = s.RoomMap[roomId]
		s.roomMtx.RUnlock()
		if ok {
			continue
		}
		break
	}
	log.Println("MAKE ROOM ID.. ", roomId)
	//Making roomMap and adding this as first Member
	s.roomMtx.Lock()
	s.RoomMap[roomId] = []string{req.Token}
	s.roomMtx.Unlock()
	log.Println("MADE ROOM AND ADDED THIS USERS ", s.RoomMap)
	return &pb.MakeRoomResponse{RoomId: roomId}, nil
}

func (s *prime) JoinRoom(_ context.Context, req *pb.JoinRoomRequest) (*pb.JoinRoomResponse, error) {
	log.Println("JOIN ROOM Hit...")
	// check token validation
	var ok bool

	s.nameMtx.RLock()
	_, ok = s.NameMap[req.Token]
	s.nameMtx.RUnlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "token not found")
	}
	log.Println("TOKEN VALIDATED..")

	// room with this room id exists ?
	s.roomMtx.RLock()
	members, ok := s.RoomMap[req.RoomId]
	s.roomMtx.RUnlock()
	if !ok {
		log.Println("ROOM DOES NOT EXITS WITH THIS ID ", req.RoomId)
		return &pb.JoinRoomResponse{JointState: pb.JoinState_UNKNOWN}, nil
	}
	log.Println("ROOM EXITS WITH THIS ID ", req.RoomId)

	// if room id is valid check if the user is already present
	for _, member := range members {
		if member == req.Token {
			return &pb.JoinRoomResponse{JointState: pb.JoinState_ALREADY}, nil
		}
	}
	log.Println("ROOM MEMBER NOT PRESENT ", s.RoomMap)

	//if the user is not present add him
	members = append(members, req.Token)
	s.roomMtx.Lock()
	s.RoomMap[req.RoomId] = members
	s.roomMtx.Unlock()
	log.Println("NEW MEMBER ADDED ", s.RoomMap)

	return &pb.JoinRoomResponse{JointState: pb.JoinState_ADDED}, nil
}

func (s *prime) Stream(srv pb.WeWatch_StreamServer) error {
	log.Println("STREAM HIT ...")
	var (
		token string
		name  string
	)

	// extracting token from context
	md, ok := metadata.FromIncomingContext(srv.Context())
	if !ok || len(md[tokenHeader]) == 0 {
		return status.Error(codes.Unauthenticated, "missing token header")
	}
	token = md[tokenHeader][0]


	s.nameMtx.RLock()
	name, ok = s.NameMap[token]
	s.nameMtx.RUnlock()
	if !ok {
		return status.Error(codes.Unauthenticated, "invalid token")
	}

	log.Println("TOKEN VALIDATED..")

	go s.sendBroadCasts(srv, token, name)

	for {

		req, err := srv.Recv()
		log.Println("STREAM READING.. ", err)
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		s.BroadCast <- pb.StreamResponse{
			Timestamp: ptypes.TimestampNow(),
			Event:     &pb.StreamResponse_ClientMessage{ClientMessage: &pb.Message{Name: name, PlayerState: req.PlayerState}},
		}
	}

	<-srv.Context().Done()
	return srv.Context().Err()
}

func (s *prime) sendBroadCasts(srv pb.WeWatch_StreamServer, tkn, name string) {
	log.Println("BROADCAST..")
	// opening the stream
	stream := s.openStream(tkn, name)
	log.Println("STREAM OPEN")
	//closing the stream
	defer s.closeStream(tkn, name)

	for {
		select {
		case <-srv.Context().Done():
			log.Println("STREAM CONTEXT DONE...")
			return
		case res := <-stream:
			log.Println("GOT SOMETHING IN STREAM SENDING...")
			if s, ok := status.FromError(srv.Send(&res)); ok {
				log.Println(s.Code().String())
				switch s.Code() {
				case codes.OK:
					// noop
				case codes.Unavailable, codes.Canceled, codes.DeadlineExceeded:
					return
				default:
					return
				}
			}
		}
	}
}

func (s *prime) openStream(tkn, name string) (stream chan pb.StreamResponse) {
	stream = make(chan pb.StreamResponse, 1000)

	s.streamMtx.Lock()
	s.StreamMap[tkn] = stream
	s.streamMtx.Unlock()

	DebugLogf("opened stream for client %s of token %s", name, tkn)
	return
}

func (s *prime) closeStream(tkn, name string) {
	s.streamMtx.Lock()
	if stream, ok := s.StreamMap[tkn]; ok {
		delete(s.StreamMap, tkn)
		close(stream)
	}
	DebugLogf("closed stream for client %s of token %s", name, tkn)
	s.streamMtx.Unlock()
}
