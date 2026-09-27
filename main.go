// Command shadowd is a Linux "shadow copy" style service: it takes
// content-deduplicated snapshots of files on a cron schedule, and lets you
// list or restore any previous version from the CLI or a web dashboard.
//
// Usage:
//
//	shadowd daemon -config shadowd.json [-listen :8080]   # run forever, snapshot on schedule (+ optional web UI)
//	shadowd serve  -config shadowd.json -listen :8080      # web UI only, no cron loop
//	shadowd snapshot -config shadowd.json                  # take one snapshot right now, then exit
//	shadowd list -config shadowd.json <file>                # show every recorded version of a file
//	shadowd restore -config shadowd.json <file> -out <dest> [-at RFC3339]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"shadowd/config"
	"shadowd/cronsched"
	"shadowd/manifest"
	"shadowd/snapshot"
	"shadowd/store"
	"shadowd/webui"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "daemon":
		cmdDaemon(os.Args[2:])
	case "serve":
		cmdServe(os.Args[2:])
	case "snapshot":
		cmdSnapshot(os.Args[2:])
	case "list", "versions":
		cmdList(os.Args[2:])
	case "restore":
		cmdRestore(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `shadowd — cron-scheduled shadow copies for individual files

Commands:
  daemon    -config FILE [-listen ADDR]         run forever, snapshotting on the configured schedule;
                                                 pass -listen (e.g. :8080) to also serve the web dashboard
  serve     -config FILE -listen ADDR           serve only the web dashboard, no cron loop
                                                 (use this if something else already runs the snapshots)
  snapshot  -config FILE                        take one snapshot of every configured path, then exit
  list      -config FILE PATH                   show every recorded version of PATH
  restore   -config FILE PATH -out DEST [-at TIME]   write a previous version of PATH to DEST
                                                 TIME is RFC3339 (e.g. 2026-09-25T14:00:00Z); omit for latest`)
}

func loadEngine(cfgPath string) (*snapshot.Engine, *config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.New(cfg.StoreDir)
	if err != nil {
		return nil, nil, err
	}
	mf, err := manifest.Load(cfg.ManifestPath)
	if err != nil {
		return nil, nil, err
	}
	return &snapshot.Engine{Store: st, Manifest: mf}, cfg, nil
}

func cmdDaemon(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	cfgPath := fs.String("config", "shadowd.json", "path to config file")
	listen := fs.String("listen", "", "if set (e.g. :8080), also serve the web dashboard on this address")
	fs.Parse(args)

	eng, cfg, err := loadEngine(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	sched, err := cronsched.Parse(cfg.Schedule)
	if err != nil {
		log.Fatalf("bad schedule %q: %v", cfg.Schedule, err)
	}

	log.Printf("shadowd starting: %d path(s), schedule %q, store %s", len(cfg.Paths), cfg.Schedule, cfg.StoreDir)

	if *listen != "" {
		startWebUI(eng, cfg, *listen)
	}

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		close(stop)
	}()

	runOnce := func() error {
		for _, p := range cfg.Paths {
			v, changed, err := eng.Take(p)
			if err != nil {
				log.Printf("snapshot %s: %v", p, err)
				continue
			}
			if changed {
				log.Printf("snapshot %s: new version at %s (+%s new data)",
					p, time.Unix(v.Timestamp, 0).Format(time.RFC3339), humanBytes(v.NewBytes))
			} else {
				log.Printf("snapshot %s: unchanged, skipped", p)
			}
		}
		return nil
	}

	// Take one snapshot immediately on startup, then follow the schedule —
	// otherwise a daily cron means waiting up to 24h to see it work at all.
	runOnce()
	sched.Run(stop, runOnce, func(err error) { log.Printf("scheduler error: %v", err) })
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "shadowd.json", "path to config file")
	listen := fs.String("listen", ":8080", "address to serve the web dashboard on")
	fs.Parse(args)

	eng, cfg, err := loadEngine(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("shadowd web UI only (no cron loop) on %s — reading %s", *listen, cfg.ManifestPath)
	log.Fatal(http.ListenAndServe(*listen, webui.NewHandler(eng, cfg)))
}

// startWebUI launches the dashboard in the background alongside the cron
// loop. It logs and continues rather than exiting the process if the port
// is already taken — a UI failing to bind shouldn't stop snapshots from
// still running on schedule.
func startWebUI(eng *snapshot.Engine, cfg *config.Config, addr string) {
	go func() {
		log.Printf("web dashboard listening on %s", addr)
		if err := http.ListenAndServe(addr, webui.NewHandler(eng, cfg)); err != nil {
			log.Printf("web dashboard stopped: %v", err)
		}
	}()
}

func cmdSnapshot(args []string) {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	cfgPath := fs.String("config", "shadowd.json", "path to config file")
	fs.Parse(args)

	eng, cfg, err := loadEngine(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	for _, p := range cfg.Paths {
		v, changed, err := eng.Take(p)
		if err != nil {
			log.Printf("snapshot %s: %v", p, err)
			continue
		}
		if changed {
			fmt.Printf("%s: new version at %s (+%s new data)\n",
				p, time.Unix(v.Timestamp, 0).Format(time.RFC3339), humanBytes(v.NewBytes))
		} else {
			fmt.Printf("%s: unchanged, skipped\n", p)
		}
	}
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	cfgPath := fs.String("config", "shadowd.json", "path to config file")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: shadowd list -config FILE PATH")
		os.Exit(1)
	}

	eng, _, err := loadEngine(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	abs, _ := filepath.Abs(fs.Arg(0))
	versions := eng.Manifest.List(abs)
	if len(versions) == 0 {
		fmt.Printf("no recorded versions of %s\n", abs)
		return
	}

	fmt.Printf("%-4s  %-25s  %-10s  %s\n", "#", "TIME", "SIZE", "NEW DATA")
	for i, v := range versions {
		fmt.Printf("%-4d  %-25s  %-10s  %s\n",
			i, time.Unix(v.Timestamp, 0).Format(time.RFC3339), humanBytes(v.Size), humanBytes(v.NewBytes))
	}
}

func cmdRestore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	cfgPath := fs.String("config", "shadowd.json", "path to config file")
	out := fs.String("out", "", "destination path to write the restored file")
	at := fs.String("at", "", "restore the version at or before this RFC3339 time (default: latest)")
	fs.Parse(args)

	if fs.NArg() < 1 || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: shadowd restore -config FILE PATH -out DEST [-at TIME]")
		os.Exit(1)
	}

	eng, _, err := loadEngine(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	var t time.Time
	if *at != "" {
		t, err = time.Parse(time.RFC3339, *at)
		if err != nil {
			log.Fatalf("bad -at time %q (want RFC3339, e.g. 2026-09-25T14:00:00Z): %v", *at, err)
		}
	}

	v, err := eng.Restore(fs.Arg(0), t, *out)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("restored version from %s -> %s\n", time.Unix(v.Timestamp, 0).Format(time.RFC3339), *out)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
