package main

import (
	"context"
	"flag"
	"log"
	"math/rand"
	"os"
	"time"
)


var (
	serverMode bool
	debugMode  bool
	host       string
	password   string
	username   string
)

func init() {
	flag.BoolVar(&serverMode, "s", false, "run as the server")
	flag.BoolVar(&debugMode, "v", false, "enable debug logging")
	flag.StringVar(&host, "h", "0.0.0.0:6262", "the chat server's host")
	flag.StringVar(&password, "p", "", "the chat server's password")
	flag.StringVar(&username, "n", "", "the username for the client")
	flag.Parse()
}

func init() {
	rand.Seed(time.Now().UnixNano())
	log.SetFlags(0)
}


func main() {
	ctx := SingleContext(context.Background())
	DebugLogf("Starting server...")
	if err := Server(":5000", "cloudwalker").Run(ctx); err != nil {
		MessageLog(time.Now(), "<<Process>>", err.Error())
		os.Exit(1)
	}
}
