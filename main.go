package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kardianos/service"
)

var (
	apps = map[string]struct {
		p *httputil.ReverseProxy
		c *exec.Cmd
		t time.Time
	}{}
	mu      sync.Mutex
	root    = ""
	domain  = ""
	port    = ""
	idleTTL = 10 * time.Minute
)

func freePort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitPort(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", addr)
}

func start(name string) (*httputil.ReverseProxy, *exec.Cmd, error) {
	f, err := os.Open(filepath.Join(root, name, "Procfile"))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var cmdStr string
	s := bufio.NewScanner(f)
	for s.Scan() {
		if strings.HasPrefix(s.Text(), "web:") {
			cmdStr = strings.TrimSpace(s.Text()[4:])
			break
		}
	}
	if cmdStr == "" {
		return nil, nil, fmt.Errorf("no web entry")
	}
	fp := freePort()
	log.Printf("START: PWD=%s PORT=%d %s", filepath.Join(root, name), fp, cmdStr)
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir, cmd.Env = filepath.Join(root, name), append(os.Environ(), fmt.Sprintf("PORT=%d", fp))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	if err := waitPort(fp, 5*time.Second); err != nil {
		return nil, nil, err
	}
	u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", fp))
	return httputil.NewSingleHostReverseProxy(u), cmd, nil
}

func handler(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimSuffix(strings.Split(r.Host, ":")[0], domain), ".")
	if name == "" {
		name = "www"
	}
	dir := filepath.Join(root, name)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "Procfile")); os.IsNotExist(err) {
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
		return
	}
	mu.Lock()
	a, ok := apps[name]
	if !ok {
		p, c, err := start(name)
		if err != nil {
			mu.Unlock()
			http.Error(w, err.Error(), 500)
			return
		}
		a = struct {
			p *httputil.ReverseProxy
			c *exec.Cmd
			t time.Time
		}{p, c, time.Now()}
	}
	a.t = time.Now()
	apps[name] = a
	mu.Unlock()
	a.p.ServeHTTP(w, r)
}

type program struct{}

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *program) run() {
	if strings.HasPrefix(root, "~") {
		root = filepath.Join(os.Getenv("HOME"), root[1:])
	}
	go func() {
		for range time.Tick(30 * time.Second) {
			mu.Lock()
			for n, a := range apps {
				if time.Since(a.t) > idleTTL {
					log.Println("STOP:", n)
					_ = a.c.Process.Kill()
					delete(apps, n)
				}
			}
			mu.Unlock()
		}
	}()
	url := fmt.Sprintf("http://%s:%s", domain, port)
	log.Printf("Serving %s from %s/*", strings.TrimSuffix(url, ":80"), root)
	log.Fatal(http.ListenAndServe(":"+port, http.HandlerFunc(handler)))
}

func (p *program) Stop(s service.Service) error {
	return nil
}

func main() {
	enableFlag := flag.Bool("enable", false, "Enable and install service")
	disableFlag := flag.Bool("disable", false, "Disable and uninstall service")
	dirFlag := flag.String("dir", "", "Directory to serve applications from")
	hostFlag := flag.String("host", "localhost", "Domain to serve applications on")
	portFlag := flag.String("port", "80", "Port to listen on")
	userFlag := flag.String("user", os.Getenv("USER"), "Run service as user")
	flag.Usage = func() {
		fmt.Println("Usage: mux -dir <dir> [options]\n")
		fmt.Println("A simple web server for managing multiple apps.\nProxies requests to http://*.localhost to applications in <dir>/*.\nAutostarts apps with $PORT set to random port.\nApp command is configured in Procfile in format: web: <cmd>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *dirFlag == "" {
		fmt.Println("Missing argument -dir <dir>")
		os.Exit(1)
	}

	root, domain, port = *dirFlag, *hostFlag, *portFlag

	svcConfig := &service.Config{
		Name:        "mux",
		DisplayName: "Mux Web Server",
		Description: "A simple web server for managing multiple apps.",
		UserName:    *userFlag,
		Arguments:   []string{fmt.Sprintf("-dir=%s", *dirFlag), fmt.Sprintf("-host=%s", *hostFlag), fmt.Sprintf("-port=%s", *portFlag)},
	}

	prg := &program{}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatal(err)
	}

	if *enableFlag {
		err = s.Install()
		if err != nil {
			log.Printf("Failed to install service: %s", err)
		} else {
			fmt.Println("Service installed.")
		}
		err = s.Start()
		if err != nil {
			log.Printf("Failed to start service: %s", err)
		} else {
			fmt.Println("Service started.")
		}
		log.Print(append(svcConfig.Arguments, "--user="+*userFlag))
		return
	}

	if *disableFlag {
		err = s.Stop()
		if err != nil {
			log.Printf("Failed to stop service: %s", err)
		} else {
			fmt.Println("Service stopped.")
		}
		err = s.Uninstall()
		if err != nil {
			log.Printf("Failed to uninstall service: %s", err)
		} else {
			fmt.Println("Service disabled.")
		}
		return
	}

	err = s.Run()
	if err != nil {
		log.Fatal(err)
	}
}
