package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"sync"

	"github.com/go-chi/chi"
	"github.com/golang/glog"
	app_util "github.com/nilebox/kanarini/pkg/util/app"
	metric_util "github.com/nilebox/kanarini/pkg/util/metric"
	"github.com/nilebox/kanarini/pkg/util/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	defaultVersion       = "0.0"
	defaultErrorRate     = 0.5
	defaultServerAddr    = ":8080"
	defaultAuxServerAddr = ":9090"
)

type App struct {
	PrometheusRegistry metric_util.PrometheusRegistry
	ErrorRate          float64
	Version            string

	// Address to listen on
	// Defaults to port 8080
	ServerAddr string

	// Address for auxiliary server to listen on
	// Defaults to port 9090
	AuxServerAddr string

	Debug bool
}

func NewFromFlags(flagset *flag.FlagSet, arguments []string) (*App, error) {
	a := App{}

	flagset.StringVar(&a.ServerAddr, "addr", defaultServerAddr, "Port to listen on")
	flagset.StringVar(&a.AuxServerAddr, "aux-addr", defaultAuxServerAddr, "Auxiliary port to listen on")
	flagset.Float64Var(&a.ErrorRate, "error-rate", defaultErrorRate, "Error rate for HTTP requests")
	flagset.StringVar(&a.Version, "version", defaultVersion, "Version to return in the info")
	flagset.BoolVar(&a.Debug, "debug", false, "Enable debug mode")

	err := flagset.Parse(arguments)
	if err != nil {
		return nil, err
	}

	a.PrometheusRegistry = prometheus.NewPedanticRegistry()

	return &a, nil
}

func (a *App) Run(ctx context.Context) error {
	glog.V(4).Info("Starting application...")

	router := chi.NewRouter()
	server := &http.Server{
		Addr:           a.ServerAddr,
		MaxHeaderBytes: 1 << 20,
		Handler:        router,
	}
	monitor := middleware.Register(a.PrometheusRegistry, a.Version)
	router.Use(monitor.MonitorRequest)
	router.Handle("/", http.HandlerFunc(a.indexHandler))
	router.Handle("/info", http.HandlerFunc(a.infoHandler))

	// Auxiliary server
	auxServer := app_util.AuxServer{
		Addr:     a.AuxServerAddr,
		Gatherer: a.PrometheusRegistry,
		IsReady:  func() bool { return true },
		Debug:    a.Debug,
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		glog.V(4).Info("Starting metrics HTTP server...")
		defer wg.Done()
		err := auxServer.Run(ctx)
		if err != nil {
			glog.V(1).Infof("auxServer error %v", err)
		}
	}()

	go func() {
		glog.V(4).Info("Starting main HTTP server...")
		defer wg.Done()
		err := server.ListenAndServe()
		if err != nil {
			glog.V(1).Infof("main server error %v", err)
		}
	}()

	wg.Wait()
	return nil
}

func (a *App) indexHandler(w http.ResponseWriter, r *http.Request) {
	glog.V(4).Info("Handling index request")

	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	html := `
	<!DOCTYPE html>
	<html>
		<head>
			<meta charset="UTF-8">
			<title>Emoji Generator</title>
			<style rel="stylesheet" type="text/css">
				body {
 					font-size: 300px;
					text-align: center;
				}
				.outer {
  					display: table;
  					position: absolute;
  					height: 100%;
 					width: 100%;
				}
				.middle {
					display: table-cell;
					vertical-align: middle;
				}
				.inner {
					margin-left: auto;
					margin-right: auto;
					width: 400px;
				}
			</style>
			<script language="javascript">
                setInterval(function(){
					url = "/info"
					fetch(url)
						.then(response=>response.json())
						.then(data=>{
							console.log(data);
							document.getElementById("version").innerHTML = data.version;
							document.getElementById("emoji").innerHTML = data.emoji;
							document.body.style.background = data.color;
						})
				}, 1000);
			</script>
		</head>
		<body>
			<div class="outer">
				<div class="middle">
					<div class="inner">
						<div id="version">-</div>
						<div id="emoji">-</div>
					</div>
				</div>
			</div>
		</body>
	</html>`
	w.Write([]byte(html))
}

func (a *App) infoHandler(w http.ResponseWriter, r *http.Request) {
	glog.V(4).Info("Handling info request")

	w.Header().Set("Content-Type", "application/json")
	emotion := a.generateEmotion()

	info := Info{
		Version: a.Version,
		Emoji:   a.generateEmoji(emotion),
		Color:   a.getBackgroundColor(emotion),
	}
	bytes, err := json.Marshal(&info)
	if err != nil {
		glog.Errorf("failed to marshal response: %#v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	status := a.getResponseCode(emotion)
	w.WriteHeader(status)
	w.Write(bytes)
}

func (a *App) getResponseCode(emotion Emotion) int {
	switch emotion {
	case EmotionHappy:
		return http.StatusOK
	case EmotionSad:
		return http.StatusInternalServerError
	default:
		panic(fmt.Sprintf("unexpected emotion: %s", emotion))
	}
}

func (a *App) getBackgroundColor(emotion Emotion) string {
	switch emotion {
	case EmotionHappy:
		return backgroundColorGreen
	case EmotionSad:
		return backgroundColorRed
	default:
		panic(fmt.Sprintf("unexpected emotion: %s", emotion))
	}
}

func (a *App) generateEmoji(emotion Emotion) string {
	switch emotion {
	case EmotionHappy:
		return emojiCodeMap[a.getRandomItem(happyCodes)]
	case EmotionSad:
		return emojiCodeMap[a.getRandomItem(sadCodes)]
	default:
		panic(fmt.Sprintf("unexpected emotion: %s", emotion))
	}
}

func (a *App) getRandomItem(items []string) string {
	return items[rand.Intn(len(items))]
}

func (a *App) generateEmotion() Emotion {
	num := float64(rand.Intn(100) + 1)
	target := a.ErrorRate * 100
	if num > target {
		return EmotionHappy
	} else {
		return EmotionSad
	}
}
