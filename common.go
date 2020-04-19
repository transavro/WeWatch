package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const timeFormat = "03:04:05 PM"

func ClientLogf(ts time.Time, format string, args ...interface{}) {
	log.Printf("[%s] <<Client>>: "+format, append([]interface{}{ts.Format(timeFormat)}, args...)...)
}

func ServerLogf(ts time.Time, format string, args ...interface{}) {
	log.Printf("[%s] <<Server>>: "+format, append([]interface{}{ts.Format(timeFormat)}, args...)...)
}

func MessageLog(ts time.Time, name, msg string) {
	log.Printf("[%s] %s: %s", ts.Format(timeFormat), name, msg)
}

func DebugLogf(format string, args ...interface{}) {
	log.Printf("[%s] <<Debug>>: "+format, append([]interface{}{time.Now().Format(timeFormat)}, args...)...)
}

func SingleContext(ctx context.Context) context.Context {
	ctx, cancle := context.WithCancel(ctx)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		DebugLogf("Listening for shutDown signal..")
		<-sigs
		DebugLogf("ShutDown Signal Received")
		signal.Stop(sigs)
		close(sigs)
		cancle()
	}()
	return ctx
}
