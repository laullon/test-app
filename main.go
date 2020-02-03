package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	metrics "github.com/rcrowley/go-metrics"

	"github.com/cloudfoundry-samples/test-app/handlers"
	"github.com/cloudfoundry-samples/test-app/helpers"
	"github.com/cloudfoundry-samples/test-app/routes"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/rata"
	"github.com/wavefronthq/go-metrics-wavefront/reporting"
	"github.com/wavefronthq/wavefront-opentracing-sdk-go/reporter"
	"github.com/wavefronthq/wavefront-opentracing-sdk-go/tracer"
	"github.com/wavefronthq/wavefront-sdk-go/application"
	"github.com/wavefronthq/wavefront-sdk-go/senders"
)

type vcapServices struct {
	WavefrontProxy []struct {
		Credentials struct {
			Hostname         string `json:"hostname"`
			Port             int    `json:"port"`
			TracingPort      int    `json:"tracingPort"`
			DistributionPort int    `json:"distributionPort"`
		} `json:"credentials"`
	} `json:"wavefront-proxy"`
}

var message string
var quiet bool

var portsFlag = flag.String(
	"ports",
	"",
	"Comma delimited list of ports, where the app will be listening to",
)

func init() {
	flag.StringVar(&message, "message", "Hello", "The Message to Log and Display")
	flag.BoolVar(&quiet, "quiet", false, "Less Verbose Logging")
	flag.Parse()
}

func main() {
	flag.Parse()

	logger := lager.NewLogger("test-app")
	if quiet {
		logger.RegisterSink(lager.NewWriterSink(os.Stdout, lager.DEBUG))
	} else {
		logger.RegisterSink(lager.NewWriterSink(os.Stdout, lager.DEBUG))
	}

	ports := getServerPorts()
	index, err := helpers.FetchIndex()
	appName := fetchAppName()

	log.Println("VCAP_SERVICES:")
	log.Println(os.Getenv("VCAP_SERVICES"))
	var services vcapServices

	var sender senders.Sender

	json.Unmarshal([]byte(os.Getenv("VCAP_SERVICES")), &services)
	if len(services.WavefrontProxy) > 0 {
		logger.Info("'Wavefront Proxy' detected, ignoring user input")
		proxyCfg := &senders.ProxyConfiguration{
			Host:             services.WavefrontProxy[0].Credentials.Hostname,
			MetricsPort:      services.WavefrontProxy[0].Credentials.Port,
			TracingPort:      services.WavefrontProxy[0].Credentials.TracingPort,
			DistributionPort: services.WavefrontProxy[0].Credentials.DistributionPort,
		}
		sender, err = senders.NewProxySender(proxyCfg)
		if err != nil {
			logger.Error("creating sender", err)
		}
	} else {
		logger.Info("'Wavefront Proxy' NOT detected, using hardcode values")
		directCfg := &senders.DirectConfiguration{
			Server:               "https://nimba.wavefront.com",
			Token:                "6490a634-ca7d-47c1-bb04-4629f53fc98b",
			BatchSize:            10000,
			MaxBufferSize:        50000,
			FlushIntervalSeconds: 1,
		}

		sender, err = senders.NewDirectSender(directCfg)
		if err != nil {
			panic(err)
		}
	}

	appTags := application.New(appName, "PCF")
	wfReporter := reporter.New(sender, appTags, reporter.Source(appName))
	clReporter := reporter.NewConsoleSpanReporter(appName) //Specify the same source you used for the WavefrontSpanReporter
	reporter := reporter.NewCompositeSpanReporter(wfReporter, clReporter)
	tracer := tracer.New(reporter, tracer.WithSampler(tracer.RateSampler{Rate: 1000}))
	opentracing.InitGlobalTracer(tracer)

	metricsReporter := reporting.NewReporter(
		sender,
		appTags,
		reporting.Source(appName),
		reporting.Prefix(fmt.Sprintf("pcf.%s.%d", appName, index)),
		reporting.LogErrors(true),
		reporting.CustomRegistry(metrics.NewRegistry()),
	)
	counter := metricsReporter.GetOrRegisterMetric("Says", metrics.NewCounter(), map[string]string{"t": "v"}).(metrics.Counter)

	logger.Info("test-app.starting", lager.Data{"ports": ports})
	go func() {
		t := time.NewTicker(time.Second)
		for {
			<-t.C
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to fetch index: %s\n", err.Error())
			} else {
				span := opentracing.StartSpan("Says opertion")
				span.SetTag("message", message)
				counter.Inc(1)
				fmt.Println(fmt.Sprintf("%s. Says %s. on index: %d", appName, message, index))
				time.Sleep(time.Duration(rand.Intn(500)) * time.Millisecond)
				span.Finish()
			}
		}
	}()

	wg := sync.WaitGroup{}
	for _, port := range ports {
		wg.Add(1)
		go func(wg *sync.WaitGroup, port string) {
			defer wg.Done()
			handler, err := rata.NewRouter(routes.Routes, handlers.New(logger, port))
			if err != nil {
				logger.Fatal("router.creation.failed", err)
			}

			server := ifrit.Envoke(http_server.New(":"+port, handler))
			logger.Info("test-app.up", lager.Data{"port": port})
			err = <-server.Wait()
			if err != nil {
				logger.Error("shutting down server", err, lager.Data{"server port": port})
			}
			logger.Info("shutting down server", lager.Data{"server port": port})
		}(&wg, port)
	}
	wg.Wait()
	logger.Info("shutting latice app")
}

func fetchAppName() string {
	appName := os.Getenv("APP_NAME")
	if appName == "" {
		return "test-app"
	}
	return appName
}

func getServerPorts() []string {
	givenPorts := *portsFlag
	if givenPorts == "" {
		givenPorts = os.Getenv("PORT")
	}
	if givenPorts == "" {
		givenPorts = "8080"
	}
	ports := strings.Replace(givenPorts, " ", "", -1)
	return strings.Split(ports, ",")
}
