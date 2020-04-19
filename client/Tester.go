package main

import (
	chat "WeWatch/proto"
	"bufio"
	"context"
	"flag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"io"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	username string
)

const tokenHeader = "x-chat-token"

func init() {
	flag.StringVar(&username, "n", "", "the username for the client")
	flag.Parse()
}

func init() {
	rand.Seed(time.Now().UnixNano())
	log.SetFlags(0)
}

func main() {
	connCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(connCtx, "192.168.0.106:5000", grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	client := chat.NewWeWatchClient(conn)

	resp, err := client.Login(context.Background(), &chat.LoginRequest{
		Password: "cloudwalker",
		Name:     username,
	})
	if err != nil {
		panic(err)
	}
	log.Println(resp.String())

	err = stream(client, context.Background(), resp.Token)
	if err != nil {
		panic(err)
	}

}

func MakeRoomReq(client chat.WeWatchClient, token string) {
	respMakeRoom, err := client.MakeRoom(context.Background(), &chat.MakeRoomRequest{
		Token: token,
	})

	if err != nil {
		panic(err)
	}
	log.Println(respMakeRoom.String())
}

func JoinRoom(client chat.WeWatchClient, token string, roomId string) {
	r, _ := strconv.Atoi(roomId)
	log.Println("room id ", r)
	resp, err := client.JoinRoom(context.Background(), &chat.JoinRoomRequest{
		Token:  token,
		RoomId: (int32(r)),
	})

	if err != nil {
		panic(err)
	}
	log.Println(resp.String())
}



func stream(wc chat.WeWatchClient , ctx context.Context, token string) error {
	md := metadata.New(map[string]string{tokenHeader: token})
	ctx = metadata.NewOutgoingContext(ctx, md)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	client, err := wc.Stream(ctx)
	if err != nil {
		return err
	}
	defer client.CloseSend()

	log.Println(time.Now(), "connected to stream")

	go send(client, wc, token)
	return receive(client)
}

func receive(sc chat.WeWatch_StreamClient) error {
	for {
		res, err := sc.Recv()

		if s, ok := status.FromError(err); ok && s.Code() == codes.Canceled {
			log.Println("stream canceled (usually indicates shutdown)")
			return nil
		} else if err == io.EOF {
			log.Println("stream closed by server")
			return nil
		} else if err != nil {
			return err
		}


		switch evt := res.Event.(type) {
		case *chat.StreamResponse_ClientLogin:
			log.Println( "%s has logged in", evt.ClientLogin.Name)
		case *chat.StreamResponse_ClientLogout:
			log.Println("%s has logged out", evt.ClientLogout.Name)
		case *chat.StreamResponse_ClientMessage:
			log.Println( evt.ClientMessage.Name, evt.ClientMessage.Message)
		case *chat.StreamResponse_ServerShutdown:
			log.Println( "the server is shutting down")
		default:
			log.Println( "unexpected event from the server: %T", evt)
			return nil
		}
	}
}


func send(client chat.WeWatch_StreamClient,actualclient chat.WeWatchClient,   token string) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Split(bufio.ScanLines)

	for {
		if sc.Scan() {
			if sc.Text() == "makeroom" {
				MakeRoomReq(actualclient, token)
			} else if strings.Contains(sc.Text(), "joinroom") {
				JoinRoom(actualclient, token, strings.Split(sc.Text(), " ")[1])
			} else if sc.Text() == "logout" {
				actualclient.Logout(context.Background(), &chat.LogoutRequest{Token: token})
				os.Exit(0)
			} else {
				err := client.Send(&chat.StreamRequest{Message: sc.Text()})
				if err != nil {
					panic(err)
				}
			}
		}
	}
}


