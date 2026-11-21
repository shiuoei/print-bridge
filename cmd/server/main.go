package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shiuoei/print-bridge/internal/agent"
	"github.com/shiuoei/print-bridge/internal/command"
	"github.com/shiuoei/print-bridge/internal/ipc"
	"github.com/shiuoei/print-bridge/internal/logging"
	"github.com/shiuoei/print-bridge/internal/printer"
	"github.com/shiuoei/print-bridge/internal/store"
)

func main() {
	logBuffer := logging.NewBuffer(500)
	// Install wraps slog's console handler rather than slog's default handler.
	// Reading slog.Default().Handler() here and handing it back would make
	// slog's defaultHandler forward into the logger that replaced it, so the
	// first record takes log's output mutex twice and every later log call in
	// the process blocks forever.
	logging.Install(logBuffer)
	dataDir := flag.String("data-dir", "", "independent PrintBridge data directory")
	flag.Parse()
	if flag.NArg() != 1 || flag.Arg(0) != "serve" {
		fmt.Fprintln(os.Stderr, "usage: print-bridge-server [-data-dir path] serve")
		os.Exit(2)
	}
	dir := *dataDir
	if dir == "" {
		var err error
		dir, err = store.DefaultDir()
		if err != nil {
			log.Fatal(err)
		}
	}
	s, err := store.Open(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	p := printer.New()
	a := agent.New(s, p)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Start(ctx); err != nil {
		log.Fatal(err)
	}
	ln, err := ipc.Listen(dir)
	if err != nil {
		log.Fatal(err)
	}
	go ipc.Serve(ctx, ln, command.Service{Store: s, Printer: p, Agent: a, Logs: logBuffer})
	notifySystemd("READY=1")
	log.Printf("PrintBridge running on port %d", s.Config().Service.Port)
	<-ctx.Done()
	notifySystemd("STOPPING=1")
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.Shutdown(shutdown); err != nil {
		log.Print(err)
	}
}
