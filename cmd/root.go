package cmd

import (
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof" // pprof handler
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/evcc-io/evcc/core"
	"github.com/evcc-io/evcc/core/keys"
	"github.com/evcc-io/evcc/push"
	"github.com/evcc-io/evcc/server"
	"github.com/evcc-io/evcc/server/mcp"
	"github.com/evcc-io/evcc/server/updater"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/auth"
	"github.com/evcc-io/evcc/util/pipe"
	"github.com/evcc-io/evcc/util/sponsor"
	"github.com/evcc-io/evcc/util/telemetry"
	_ "github.com/joho/godotenv/autoload"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cast"
	"github.com/spf13/cobra"
	vpr "github.com/spf13/viper"
)

const (
	rebootDelay = 15 * time.Minute // delayed reboot on error
	serviceDB   = "/var/lib/evcc/evcc.db"
	userDB      = "~/.evcc/evcc.db"
)

var (
	log           = util.NewLogger("main")
	cfgFile       string
	cfgDatabase   string
	customCssFile string
	ignoreEmpty   = ""                                      // ignore empty keys
	ignoreLogs    = []string{"log"}                         // ignore log messages, including warn/error
	ignoreMqtt    = []string{"log", "auth", "releaseNotes"} // excessive size may crash certain brokers

	viper *vpr.Viper

	runAsService bool
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:     "evcc",
	Short:   "evcc - open source solar charging",
	Version: util.FormattedVersion(),
	Run:     runRoot,
}

func init() {
	viper = vpr.NewWithOptions(vpr.ExperimentalBindStruct())

	viper.SetEnvPrefix("evcc")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv() // read in environment variables that match

	util.LogLevel("info", nil)

	cobra.OnInitialize(initConfig)

	// global options
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "Config file (default \"~/evcc.yaml\" or \"/etc/evcc.yaml\")")
	rootCmd.PersistentFlags().StringVar(&cfgDatabase, "database", "", "Database location (default \"~/.evcc/evcc.db\")")
	rootCmd.PersistentFlags().BoolP("help", "h", false, "Help")
	rootCmd.PersistentFlags().Bool(flagDemoMode, false, flagDemoModeDescription)
	rootCmd.PersistentFlags().Bool(flagHeaders, false, flagHeadersDescription)
	rootCmd.PersistentFlags().Bool(flagIgnoreDatabase, false, flagIgnoreDatabaseDescription)
	rootCmd.PersistentFlags().String(flagTemplate, "", flagTemplateDescription)
	rootCmd.PersistentFlags().String(flagTemplateType, "", flagTemplateTypeDescription)
	rootCmd.PersistentFlags().StringVar(&customCssFile, flagCustomCss, "", flagCustomCssDescription)

	// config file options
	rootCmd.PersistentFlags().StringP("log", "l", "info", "Log level (fatal, error, warn, info, debug, trace)")
	bindP(rootCmd, "log")

	rootCmd.Flags().Bool("metrics", false, "Expose metrics")
	bind(rootCmd, "metrics")

	rootCmd.Flags().Bool("profile", false, "Expose pprof profiles")
	bind(rootCmd, "profile")

	rootCmd.Flags().Bool("mcp", false, "Expose MCP service (experimental)")
	bind(rootCmd, "mcp")

	rootCmd.Flags().Bool(flagDisableAuth, false, flagDisableAuthDescription)
}

// initConfig reads in config file and ENV variables if set
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag
		viper.SetConfigFile(cfgFile)
	} else {
		// Search for config in home directory if available
		if home, err := os.UserHomeDir(); err == nil {
			viper.AddConfigPath(home)
		}

		// Search config in home directory with name "mbmd" (without extension).
		viper.AddConfigPath(".")    // optionally look for config in the working directory
		viper.AddConfigPath("/etc") // path to look for the config file in

		viper.SetConfigName("evcc")
	}
	if cfgDatabase != "" {
		viper.Set("Database.Dsn", cfgDatabase)
	}
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func runRoot(cmd *cobra.Command, args []string) {
	runAsService = true

	// print version
	log.INFO.Printf("evcc %s", util.FormattedVersion())

	// load config and re-configure logging after reading config file
	var err error
	if ok, _ := cmd.Flags().GetBool(flagDemoMode); ok {
		log.INFO.Println("switching into demo mode as requested through the --demo flag")
		if err := demoConfig(&conf); err != nil {
			log.FATAL.Fatal(err)
		}
	} else {
		if cfgErr := loadConfigFile(&conf, !cmd.Flag(flagIgnoreDatabase).Changed); cfgErr != nil {
			// evcc.yaml found, might have errors
			err = wrapErrorWithClass(ClassConfigFile, cfgErr)
		}
	}

	// setup environment
	if err == nil {
		err = configureEnvironment(cmd, &conf)
	}

	// configure network
	if err == nil {
		err = networkSettings(&conf.Network)
	}

	log.INFO.Printf("UI listening at :%d", conf.Network.Port)

	// start broadcasting values
	tee := new(util.Tee)
	valueChan := make(chan util.Param, 64)
	go tee.Run(valueChan)

	// value cache
	cache := util.NewParamCache()
	go cache.Run(pipe.NewDropper(ignoreLogs...).Pipe(tee.Attach()))

	// create web server
	socketHub := server.NewSocketHub()
	httpd := server.NewHTTPd(fmt.Sprintf(":%d", conf.Network.Port), socketHub, customCssFile)

	// metrics
	if viper.GetBool("metrics") {
		httpd.Router().Handle("/metrics", promhttp.Handler())
	}

	// pprof
	if viper.GetBool("profile") {
		httpd.Router().PathPrefix("/debug/").Handler(http.DefaultServeMux)
	}

	// publish to UI
	go socketHub.Run(pipe.NewDropper(ignoreEmpty).Pipe(tee.Attach()), cache)

	// capture log messages for UI
	util.CaptureLogs(valueChan)

	// setup telemetry
	if err == nil {
		telemetry.Create(conf.Plant, valueChan)
		if conf.Telemetry {
			err = telemetry.Enable(true)
		}
	}

	// setup modbus proxy
	if err == nil {
		err = wrapErrorWithClass(ClassModbusProxy, configureModbusProxy(&conf.ModbusProxy))
	}

	// setup site and loadpoints
	var site *core.Site
	if err == nil {
		site, err = configureSiteAndLoadpoints(&conf)
	}

	// setup influx
	if err == nil {
		influx, ierr := configureInflux(&conf.Influx)
		if ierr != nil {
			err = wrapErrorWithClass(ClassInflux, ierr)
		}

		if err == nil && influx != nil {
			// eliminate duplicate values
			dedupe := pipe.NewDeduplicator(30*time.Minute,
				keys.VehicleSoc,
				keys.VehicleRange,
				keys.VehicleOdometer,
				keys.TariffCo2,
				keys.TariffCo2Home,
				keys.TariffCo2Loadpoints,
				keys.TariffFeedIn,
				keys.TariffGrid,
				keys.TariffPriceHome,
				keys.TariffPriceLoadpoints,
				keys.TariffSolar,
				keys.ChargedEnergy,
				keys.ChargeRemainingEnergy)
			go influx.Run(site, dedupe.Pipe(
				pipe.NewDropper(append(ignoreLogs, ignoreEmpty, keys.Forecast)...).Pipe(tee.Attach()),
			))
		}
	}

	// signal restart
	valueChan <- util.Param{Key: keys.Startup, Val: true}

	// setup mqtt publisher
	if err == nil && conf.Mqtt.Broker != "" && conf.Mqtt.Topic != "" {
		var mqtt *server.MQTT
		mqtt, err = server.NewMQTT(strings.Trim(conf.Mqtt.Topic, "/"), site)
		if err == nil {
			go mqtt.Run(site, pipe.NewDropper(append(ignoreMqtt, ignoreEmpty)...).Pipe(tee.Attach()))
		}
	}

	// announce on mDNS
	if err == nil && strings.HasSuffix(conf.Network.Host, ".local") {
		err = configureMDNS(conf.Network)
	}

	// start HEMS server
	if err == nil {
		err = wrapErrorWithClass(ClassHEMS, configureHEMS(&conf.HEMS, site, httpd))
	}

	// setup MCP
	if viper.GetBool("mcp") {
		const path = "/mcp"
		local := conf.Network.URI()
		router := httpd.Router()

		var handler http.Handler
		if handler, err = mcp.NewHandler(router, local, path); err == nil {
			router.PathPrefix(path).Handler(handler)
		}
	}

	// setup messaging
	var pushChan chan push.Event
	if err == nil {
		pushChan, err = configureMessengers(&conf.Messaging, site.Vehicles(), valueChan, cache)
		err = wrapErrorWithClass(ClassMessenger, err)
	}

	// publish initial settings
	valueChan <- util.Param{Key: keys.EEBus, Val: conf.EEBus.Configured()}
	valueChan <- util.Param{Key: keys.Hems, Val: conf.HEMS}
	valueChan <- util.Param{Key: keys.Influx, Val: conf.Influx}
	valueChan <- util.Param{Key: keys.Interval, Val: conf.Interval}
	valueChan <- util.Param{Key: keys.Messaging, Val: conf.Messaging.Configured()}
	valueChan <- util.Param{Key: keys.ModbusProxy, Val: conf.ModbusProxy}
	valueChan <- util.Param{Key: keys.Mqtt, Val: conf.Mqtt}
	valueChan <- util.Param{Key: keys.Network, Val: conf.Network}
	valueChan <- util.Param{Key: keys.Sponsor, Val: sponsor.Status()}

	// run shutdown functions on stop
	var once sync.Once
	stopC := make(chan struct{})

	// catch signals
	go func() {
		signalC := make(chan os.Signal, 1)
		signal.Notify(signalC, os.Interrupt, syscall.SIGTERM)

		<-signalC                        // wait for signal
		once.Do(func() { close(stopC) }) // signal loop to end
	}()

	// wait for shutdown
	go func() {
		<-stopC

		select {
		case <-shutdownDoneC(): // wait for shutdown
		case <-time.After(conf.Interval):
		}

		// exit code 1 on error
		os.Exit(cast.ToInt(err != nil))
	}()

	// allow web access for vehicles
	configureAuth(httpd.Router())

	authObject := auth.New()
	if ok, _ := cmd.Flags().GetBool(flagDisableAuth); ok {
		log.WARN.Println("❗❗❗ Authentication is disabled. This is dangerous. Your data and credentials are not protected.")
		authObject.SetAuthMode(auth.Disabled)
		valueChan <- util.Param{Key: keys.AuthDisabled, Val: true}
	}

	if ok, _ := cmd.Flags().GetBool(flagDemoMode); ok {
		log.WARN.Println("Authentication is locked in demo mode. Login-features are disabled.")
		authObject.SetAuthMode(auth.Locked)
		valueChan <- util.Param{Key: keys.DemoMode, Val: true}
	}

	httpd.RegisterSystemHandler(site, valueChan, cache, authObject, func() {
		log.INFO.Println("evcc was stopped by user. OS should restart the service. Or restart manually.")
		err = errors.New("restart required") // https://gokrazy.org/development/process-interface/
		once.Do(func() { close(stopC) })     // signal loop to end
	})

	// show and check version, reduce api load during development
	if util.Version != util.DevVersion {
		valueChan <- util.Param{Key: keys.Version, Val: util.FormattedVersion()}
		go updater.Run(log, httpd, valueChan)
	}

	// setup site
	if err == nil {
		// set channels
		site.DumpConfig()
		site.Prepare(valueChan, pushChan)

		httpd.RegisterSiteHandlers(site, valueChan)

		go func() {
			site.Run(stopC, conf.Interval)
		}()
	}

	if err != nil {
		// improve error message
		err = wrapFatalError(err)
		valueChan <- util.Param{Key: keys.Fatal, Val: err}

		// TODO stop reboot loop if user updates config (or show countdown in UI)
		log.FATAL.Println(err)
		log.FATAL.Printf("will attempt restart in: %v", rebootDelay)

		go func() {
			<-time.After(rebootDelay)
			once.Do(func() { close(stopC) }) // signal loop to end
		}()
	}

	// uds health check listener
	go server.HealthListener(site)

	log.FATAL.Println(wrapFatalError(httpd.ListenAndServe()))
}
