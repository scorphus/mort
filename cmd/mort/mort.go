package main

import (
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"go.uber.org/zap"

	"github.com/aldor007/mort/pkg/config"
	"github.com/aldor007/mort/pkg/lock"
	mortMiddleware "github.com/aldor007/mort/pkg/middleware"
	"github.com/aldor007/mort/pkg/monitoring"
	"github.com/aldor007/mort/pkg/object"
	"github.com/aldor007/mort/pkg/processor"
	"github.com/aldor007/mort/pkg/response"
	"github.com/aldor007/mort/pkg/throttler"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

const (
	// Version of mort
	Version = "0.6.0"
	// BANNER just fancy command line banner
	BANNER = `
  /\/\   ___  _ __| |_
 /    \ / _ \| '__| __|
/ /\/\ \ (_) | |  | |_
\/    \/\___/|_|   \__|
 Version: %s
`
)

func debugListener(mortConfig *config.Config) (s *http.Server, ln net.Listener, socketPath string) {
	router := chi.NewRouter()
	router.Mount("/debug", middleware.Profiler())
	router.Handle("/metrics", promhttp.Handler())
	s = &http.Server{
		ReadTimeout:  2 * time.Minute,
		WriteTimeout: 2 * time.Minute,
		Handler:      router,
	}

	network := "tcp"
	address := mortConfig.Server.InternalListen
	socketPath = ""
	if strings.HasPrefix(address, "unix:") {
		network = "unix"
		socketPath = address
		address = strings.Replace(address, "unix:", "", 1)
	}

	ln, err := net.Listen(network, address)
	if err != nil {
		panic(err)
	}

	return
}

func handleSignals(servers []*http.Server, socketPaths []string, wg *sync.WaitGroup) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGUSR2, syscall.SIGKILL, syscall.SIGINT, syscall.SIGTERM, os.Kill)
	for {
		sig := <-signalChan
		switch sig {
		case syscall.SIGTERM:
			fallthrough
		case syscall.SIGKILL:
			fallthrough
		case syscall.SIGINT:
			fallthrough
		case os.Kill:
			for _, s := range servers {
				s.Close()
				wg.Done()
			}

			for _, socketPath := range socketPaths {
				os.Remove(socketPath)
			}
			wg.Done()
			return
		default:
		}
	}
}

func configureMonitoring(mortConfig *config.Config) {
	logger, _ := zap.NewProduction()
	//logger, _ := zap.NewDevelopment()
	zap.ReplaceGlobals(logger)
	monitoring.RegisterLogger(logger)
	if mortConfig.Server.Monitoring == "prometheus" {
		p := monitoring.NewPrometheusReporter()
		p.RegisterCounterVec("cache_ratio", prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mort_cache_ratio",
			Help: "mort cache ratio",
		},
			[]string{"status"},
		))

		p.RegisterCounter("throttled_count", prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mort_request_throttled_count",
			Help: "mort count of throttled requests",
		}))

		p.RegisterCounter("collaped_count", prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mort_request_collapsed_count",
			Help: "mort count of collapsed requests",
		}))

		monitoring.RegisterReporter(p)
	}
}

func startServer(s *http.Server, ln net.Listener) {
	err := s.Serve(ln)
	if err != nil && err != http.ErrServerClosed {
		fmt.Println("Error listen", err)
	}
}

func main() {
	configPath := flag.String("config", "/etc/mort/mort.yml", "Path to configuration")
	debug := flag.Bool("debug", false, "enable debug mode")
	flag.Parse()

	router := chi.NewRouter()
	imgConfig := config.GetInstance()
	err := imgConfig.Load(*configPath)
	configureMonitoring(imgConfig)

	if err != nil {
		panic(err)
	}

	fmt.Printf(BANNER, "v"+Version)
	fmt.Printf("Config file %s listen addr %s debug: %t pid: %d \n", *configPath, imgConfig.Server.Listen, *debug, os.Getpid())

	rp := processor.NewRequestProcessor(imgConfig.Server, lock.NewMemoryLock(), throttler.NewBucketThrottler(10))
	s3Auth := mortMiddleware.NewS3AuthMiddleware(imgConfig)

	router.Use(s3Auth.Handler)

	router.Use(func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(resWriter http.ResponseWriter, req *http.Request) {
			debug := req.Header.Get("X-Mort-Debug") != ""
			obj, err := object.NewFileObject(req.URL, imgConfig)
			if err != nil {
				monitoring.Logs().Errorf("Unable to create file object err = %s", err)
				response.NewError(400, err).SetDebug(debug, nil).Send(resWriter)
				return
			}

			res := rp.Process(req, obj)
			res.SetDebug(debug, obj)
			if debug {
				res.Set("X-Mort-Version", Version)
			}

			// FIXME
			res.Set("Access-Control-Allow-Headers", "Content-Type, X-Amz-Public-Width, X-Amz-Public-Height")
			res.Set("Access-Control-Expose-Headers", "Content-Type, X-Amz-Public-Width, X-Amz-Public-Height")
			res.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, HEAD")
			defer monitoring.Log().Sync() // flushes buffer, if any
			if res.HasError() {
				monitoring.Log().Warn("Mort process error", zap.String("obj.Key", obj.Key), zap.Error(res.Error()))
			}

			res.SendContent(req, resWriter)
		})
	})

	router.HandleFunc("/", http.HandlerFunc(func(resWriter http.ResponseWriter, req *http.Request) {
		resWriter.WriteHeader(400)
		monitoring.Log().Warn("Mort error request shouldn't go here")
	}))

	serversCount := len(imgConfig.Server.Listen) + 1
	servers := make([]*http.Server, serversCount)
	netListeners := make([]net.Listener, serversCount)
	var socketPaths []string

	for i, l := range imgConfig.Server.Listen {
		servers[i] = &http.Server{
			ReadTimeout:  2 * time.Minute,
			WriteTimeout: 2 * time.Minute,
			Handler:      router,
		}

		network := "tcp"
		address := l
		if strings.HasPrefix(l, "unix:") {
			network = "unix"
			address = strings.Replace(l, "unix:", "", 1)
			socketPaths = append(socketPaths, address)
		}

		ln, err := net.Listen(network, address)
		if err != nil {
			panic(err)
		}
		netListeners[i] = ln
	}

	var internalSocketPath string
	servers[serversCount-1], netListeners[serversCount-1], internalSocketPath = debugListener(imgConfig)
	if internalSocketPath != "" {
		socketPaths = append(socketPaths, internalSocketPath)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go handleSignals(servers, socketPaths, &wg)

	for i, s := range servers {
		wg.Add(1)
		go startServer(s, netListeners[i])
	}

	wg.Wait()
	fmt.Println("Bye...")
}
