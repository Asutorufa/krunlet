package main

import (
	"context"
	"fmt"
	"github.com/Asutorufa/krunlet"
	"log"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: basic ROOTFS")
	}
	s, err := krunlet.New(krunlet.Options{RootFS: os.Args[1], Timeout: 5 * time.Second, Network: false})
	if err != nil {
		log.Fatal(err)
	}
	r, err := s.Run(context.Background(), krunlet.Request{Command: []string{"/bin/sh", "-c", "cat /workspace/task.txt > /workspace/result.txt"},
		Files: map[string][]byte{"/workspace/task.txt": []byte("hello from an agent\n")}, Collect: []string{"/workspace/result.txt"}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("exit=%d output=%q\n", r.ExitCode, r.Files["/workspace/result.txt"])
}
