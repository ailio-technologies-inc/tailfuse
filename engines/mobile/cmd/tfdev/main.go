// Command tfdev runs the Tailfuse mobile engine on a computer, for testing.
//
// It starts the engine on a state folder, then reads one JSON request per
// line on stdin and prints each response on its own line. Change events are
// printed as `event: <kind>`. Two extra lines are understood: `start` and
// `stop` (start or stop the engine, to try the "works off" commands).
//
//	go run ./cmd/tfdev -state /tmp/tf -listen 127.0.0.1:43199
//	{"cmd":"add_profile"}
//	{"cmd":"state"}
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"strings"
	"sync"

	engine "github.com/ailio-technologies-inc/tailfuse/engines/mobile"
)

func main() {
	stateDir := flag.String("state", "", "state folder (required)")
	listen := flag.String("listen", engine.DefaultListen, "router address (loopback)")
	verbose := flag.Bool("v", false, "print engine logs to stderr")
	off := flag.Bool("off", false, "do not start the engine; only the works-off commands succeed")
	iosLimits := flag.Bool("ioslimits", false, "apply the iOS Go memory limit and GC percent")
	flag.Parse()
	if *stateDir == "" {
		fmt.Fprintln(os.Stderr, "tfdev: -state is required")
		os.Exit(2)
	}
	if *iosLimits {
		limit, pct := engine.IOSMemorySettings()
		debug.SetMemoryLimit(limit)
		debug.SetGCPercent(pct)
	}
	logf := func(string, ...any) {}
	if *verbose {
		l := log.New(os.Stderr, "tfdev: ", log.LstdFlags)
		logf = l.Printf
	}
	var outMu sync.Mutex
	println := func(s string) {
		outMu.Lock()
		defer outMu.Unlock()
		fmt.Println(s)
	}
	e := engine.New(engine.Options{StateDir: *stateDir, Listen: *listen, NodeFactory: engine.TSNetFactory, Logf: logf})
	e.SetListener(func(kind string) { println("event: " + kind) })
	if !*off {
		if err := e.Start(*stateDir); err != nil {
			fmt.Fprintln(os.Stderr, "tfdev: start:", err)
			os.Exit(1)
		}
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch line {
		case "":
			continue
		case "start":
			if err := e.Start(*stateDir); err != nil {
				println(fmt.Sprintf(`{"ok":false,"error":%q}`, err.Error()))
			} else {
				println(`{"ok":true,"result":null}`)
			}
			continue
		case "stop":
			e.Stop()
			println(`{"ok":true,"result":null}`)
			continue
		}
		println(e.Call(line))
	}
	e.Stop()
}
