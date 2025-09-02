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
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/kardianos/service"
	ignore "github.com/sabhiram/go-gitignore"
)

var (
	apps    = map[string]*appInfo{}
	mu      sync.Mutex
	root    = ""
	domain  = ""
	port    = ""
	idleTTL = 10 * time.Minute
	verbose = false
)

type appInfo struct {
	name    string
	dir     string
	p       *httputil.ReverseProxy
	c       *exec.Cmd
	t       time.Time
	watcher *fsnotify.Watcher
	ig      *ignore.GitIgnore
}

const debounceDelay = 1000 * time.Millisecond

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
	return fmt.Errorf("TIMEOUT %s", addr)
}

func loadInvertedIgnore(file string) (*ignore.GitIgnore, error) {
	return ignore.CompileIgnoreFile(file)
}

func matchInverted(path string, ig *ignore.GitIgnore) bool {
	if ig == nil {
		return false
	}
	parts := strings.Split(path, string(filepath.Separator))
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			if !ig.MatchesPath(path) {
				return false
			}
		}
	}

	return ig.MatchesPath(path)
}

func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return w.Add(path)
		}
		return nil
	})
}

func start(name string) (*appInfo, error) {
	dir := filepath.Join(root, name)
	f, err := os.Open(filepath.Join(dir, "Procfile"))
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("NO web: in %s/Procfile", dir)
	}

	fp := freePort()
	if verbose {
		log.Printf("START: PWD=%s PORT=%d %s", dir, fp, cmdStr)
	}
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), fmt.Sprintf("PORT=%d", fp))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if err := waitPort(fp, 5*time.Second); err != nil {
		return nil, err
	}

	u, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", fp))
	proxy := httputil.NewSingleHostReverseProxy(u)

	app := &appInfo{
		name: name,
		dir:  dir,
		p:    proxy,
		c:    cmd,
		t:    time.Now(),
	}

	startWatcher(app)

	return app, nil
}

func stopApp(app *appInfo) {
	if verbose {
		log.Print("STOP: ", app.name)
	}
	mu.Lock()
	_ = app.c.Process.Kill()
	app.watcher.Close()
	delete(apps, app.name)
	mu.Unlock()
}

func startWatcher(app *appInfo) {
	if app.watcher != nil {
		app.watcher.Close()
	}

	ig, _ := loadInvertedIgnore(filepath.Join(app.dir, ".watch"))
	app.ig = ig

	watcher, _ := fsnotify.NewWatcher()
	_ = addRecursive(watcher, app.dir)
	app.watcher = watcher

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == filepath.Join(app.dir, ".watch") {
					if verbose {
						log.Print("UPDATED: ", event.Name)
					}
					stopApp(app)
				}
				if event.Op&fsnotify.Create == fsnotify.Create {
					if fi, err := os.Stat(event.Name); err == nil && fi.IsDir() {
						if matchInverted(event.Name, ig) {
							_ = addRecursive(watcher, event.Name)
						}
					}
				}
				if matchInverted(event.Name, ig) {
					if verbose {
						log.Print("UPDATED: ", event.Name)
					}
					stopApp(app)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Print(err)
			}
		}
	}()
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
		newApp, err := start(name)
		if err != nil {
			mu.Unlock()
			http.Error(w, err.Error(), 500)
			return
		}
		apps[name] = newApp
		a = newApp
	}
	a.t = time.Now()
	mu.Unlock()
	a.p.ServeHTTP(w, r)
}

type program struct{}

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *program) run() {
	go func() {
		for range time.Tick(30 * time.Second) {
			mu.Lock()
			for _, a := range apps {
				if time.Since(a.t) > idleTTL {
					if verbose {
						log.Print("IDLE: ", a.name)
					}
					stopApp(a)
				}
			}
			mu.Unlock()
		}
	}()
	url := fmt.Sprintf("http://%s:%s", domain, port)
	log.Printf("%s (%s)", strings.TrimSuffix(url, ":80"), root)
	log.Fatal(http.ListenAndServe(":"+port, http.HandlerFunc(handler)))
}

func (p *program) Stop(s service.Service) error {
	return nil
}

func main() {
	var err error
	var s service.Service
	flag.Usage = func() {
		fmt.Fprint(os.Stderr,
			"\n",
			"Autostarts apps and serves them at subdomains. Reloads them on changes.\n",
			"\n",
			"Setup with:\n",
			"  mux -enable\n",
			"\n",
			"Setup apps:\n",
			"  ~/Web/APP/Procfile:  web: ./start.sh $PORT\n",
			"  ~/Web/APP/.watch:    src/*\n",
			"\n",
			"Visiting http://APP.localhost will start and serve the app.\n",
			"\n",
			"Options:\n",
		)
		flag.PrintDefaults()
		fmt.Fprint(os.Stderr, "\n")
	}
	enableFlag := flag.Bool("enable", false, "start on boot")
	disableFlag := flag.Bool("disable", false, "disable start on boot")
	dirFlag := flag.String("dir", "~/Web", "directory to serve applications from")
	hostFlag := flag.String("host", "localhost", "serve on http://*.HOST")
	portFlag := flag.String("port", "7777", "port to listen on")
	verboseFlag := flag.Bool("verbose", false, "verbose logging")
	flag.Parse()

	root, domain, port, verbose = *dirFlag, *hostFlag, *portFlag, *verboseFlag
	if strings.HasPrefix(root, "~") {
		root = filepath.Join(os.Getenv("HOME"), root[1:])
	}
	root, err = filepath.Abs(root)
	if err != nil {
		log.Fatal(err)
	}

	currentUser, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}

	svcConfig := &service.Config{
		Name:        "mux",
		DisplayName: "Mux Web Server",
		Arguments: []string{
			fmt.Sprintf("-dir=%s", root),
			fmt.Sprintf("-host=%s", *hostFlag),
			fmt.Sprintf("-port=%s", *portFlag),
		},
		// UserName: currentUser.Username,
		Option: service.KeyValue{
			"UserService": currentUser.Uid != "0",
		},
	}

	prg := &program{}
	s, err = service.New(prg, svcConfig)
	if err != nil {
		log.Fatal(err)
	}

	if *enableFlag {
		if err = s.Install(); err != nil {
			log.Print(err)
		}
		if err = s.Start(); err != nil {
			log.Print(err)
		}
		log.Print(svcConfig.Arguments, currentUser.Username, currentUser.Uid != "0")
		return
	}

	if *disableFlag {
		if err = s.Stop(); err != nil {
			log.Print(err)
		}
		if err = s.Uninstall(); err != nil {
			log.Print(err)
		}
		return
	}

	if err = s.Run(); err != nil {
		log.Fatal(err)
	}
}
