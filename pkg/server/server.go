package server

import (
	"fmt"
	"github.com/labstack/echo-contrib/prometheus"
	"io"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/brpaz/echozap"
	"github.com/flosch/pongo2/v6"
	"github.com/go-resty/resty/v2"
	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/yctomwang/badgeserv/api/v1"
	"github.com/yctomwang/badgeserv/assets"
	"github.com/yctomwang/badgeserv/pkg/badges"
	"github.com/yctomwang/badgeserv/pkg/pongorenderer"
	"github.com/yctomwang/badgeserv/pkg/server/badgeconfig"
	"github.com/yctomwang/badgeserv/version"
	"go.uber.org/zap"
	"go.withmatt.com/httpheaders"
)

// APIServerConfig configures local hosting parameters of the API server.
type APIServerConfig struct {
	Prefix string `help:"Prefix the API is bing served under, if any"`
	Host   string `help:"Host the API should be served on" default:""`
	Port   int    `help:"Port to serve on" default:"8080"`

	HTTPClient APIHTTPClientConfig `embed:"" prefix:"http"`
}

// APIHTTPClientConfig configures the outbound HTTP request globals.
type APIHTTPClientConfig struct {
	Timeout   time.Duration `help:"Default HTTP request timeout" default:"3s"`
	UserAgent string        `help:"User Agent string to send with requests" default:""`
}

var (
	ErrAPIInitializationFailed = errors.New("API failed to initialize")
)

func loadBadgeConfig(badgeConfigDir string) (*badgeconfig.Config, error) {
	logger := zap.L()
	var predefinedBadgeConfig *badgeconfig.Config
	if badgeConfigDir != "" {
		logger.Info("Loading predefined badge configs")
		var err error
		predefinedBadgeConfig, err = badgeconfig.LoadDir(badgeConfigDir)
		if err != nil {
			logger.Error("Fatal error loading predefined badge configuration")
			return predefinedBadgeConfig, errors.Wrap(err, "badgeconfig")
		}
	} else {
		logger.Info("No predefined badge configs")
		predefinedBadgeConfig = &badgeconfig.Config{PredefinedBadges: map[string]badgeconfig.BadgeDefinition{}}
	}
	return predefinedBadgeConfig, nil
}

func getPredefinedBadgesTemplateData(predefinedBadgeConfig *badgeconfig.Config) interface{} {
	type templatePredefinedExample struct {
		Description string
		URL         string
	}

	type templateParameter struct {
		Name        string
		Description string
	}

	type templatePredefinedBadge struct {
		Name       string
		Examples   []templatePredefinedExample
		Parameters []templateParameter
	}

	if predefinedBadgeConfig == nil {
		return []templatePredefinedBadge{}
	}

	predefinedBadges := []templatePredefinedBadge{}
	for predefinedName, predefinedDesc := range predefinedBadgeConfig.PredefinedBadges {
		exampleDefs := []templatePredefinedExample{}
		for _, example := range predefinedDesc.Examples {
			exampleURL := url.URL{Path: fmt.Sprintf("badge/predefined/%s/", predefinedName)}
			qry := exampleURL.Query()
			for k, v := range example.Parameters {
				qry.Set(k, v)
			}
			exampleURL.RawQuery = qry.Encode()

			exampleDefs = append(exampleDefs, templatePredefinedExample{
				Description: example.Description,
				URL:         exampleURL.String(),
			})
		}

		parameterList := lo.MapToSlice(predefinedDesc.Parameters, func(k string, v string) templateParameter {
			return templateParameter{
				Name:        k,
				Description: v,
			}
		})

		sort.Slice(parameterList, func(i, j int) bool {
			return strings.Compare(parameterList[i].Name, parameterList[j].Name) < 0
		})

		predefinedBadges = append(predefinedBadges, templatePredefinedBadge{
			Name:       predefinedName,
			Examples:   exampleDefs,
			Parameters: parameterList,
		})
	}

	sort.Slice(predefinedBadges, func(i, j int) bool {
		return strings.Compare(predefinedBadges[i].Name, predefinedBadges[j].Name) < 0
	})

	return predefinedBadges
}

// API launches an ApiV1 instance server and manages it's lifecycle.
func API(serverConfig APIServerConfig, badgeConfig badges.BadgeConfig, assetConfig assets.Config, badgeConfigDir string) error {
	logger := zap.L()

	predefinedBadgeConfig, err := loadBadgeConfig(badgeConfigDir) //loads the assets
	if err != nil {
		return errors.Wrap(err, "API")
	}

	logger.Debug("Configuring API REST client")
	httpClient := resty.New()
	if serverConfig.HTTPClient.UserAgent == "" {
		httpClient.SetHeader(httpheaders.UserAgent, fmt.Sprintf("%s/%s", version.Name, version.Version))
	} else {
		httpClient.SetHeader(httpheaders.UserAgent, serverConfig.HTTPClient.UserAgent)
	}
	httpClient.SetTimeout(serverConfig.HTTPClient.Timeout)
	logger.Info("HTTP client initialized", zap.Bool("proxy_set", httpClient.IsProxySet()))

	badgeService := badges.NewBadgeService(&badgeConfig)

	logger.Debug("Creating API config")
	apiConfig := &api.Config{
		BadgeService:     badgeService,
		HTTPClient:       httpClient,
		PredefinedBadges: predefinedBadgeConfig,
	}
	apiInstance, apiPrefix := api.NewAPI(apiConfig)

	if apiInstance == nil {
		err := ErrAPIInitializationFailed
		logger.Error("API failed to initialize", zap.Error(err))
		return ErrAPIInitializationFailed
	}

	templateGlobals := make(pongo2.Context)
	templateGlobals["ApiVersionPrefix"] = apiPrefix
	templateGlobals["Version"] = map[string]string{
		"Version":     version.Version,
		"Name":        version.Name,
		"Description": version.Description,
	}
	templateGlobals["Colors"] = badgeService.Colors
	templateGlobals["PredefinedBadges"] = getPredefinedBadgesTemplateData(predefinedBadgeConfig)

	logger.Info("Starting API server")
	//inside this APIConfigure, we are passing the Implementation of the ServiceInterface
	if err := Server(serverConfig, assetConfig, templateGlobals, APIConfigure(serverConfig, apiInstance, apiPrefix)); err != nil {
		logger.Error("Error from server", zap.Error(err))
		return errors.Wrap(err, "Server exiting with error")
	}

	return nil
}

// APIConfigure implements the logic necessary to launch an API from a server config and a server.
// The primary difference to API() is that the apInstance interface is explicitly passed.
func APIConfigure[T api.ServerInterface](serverConfig APIServerConfig, apiInstance T, apiPrefix string) func(e *echo.Echo) error {
	return func(e *echo.Echo) error {
		var logger = zap.L().With(zap.String("subsystem", "server"))

		fullAPIPrefix := fmt.Sprintf("%s/api/%s", serverConfig.Prefix, apiPrefix)
		logger.Info("Initializing API with apiPrefix",
			zap.String("configured_prefix", serverConfig.Prefix),
			zap.String("api_prefix", apiPrefix),
			zap.String("api_basepath", fullAPIPrefix))
		//here is where we actually enter the wrapper
		api.RegisterHandlersWithBaseURL(e, apiInstance, fullAPIPrefix)
		// Add the Swagger API as the frontend.
		uiPrefix := fmt.Sprintf("%s/ui", fullAPIPrefix)
		uiHandler := EchoSwaggerUIHandler(uiPrefix, api.OpenAPISpec)
		e.GET(fmt.Sprintf("%s", uiPrefix), uiHandler) //nolint:gosimple
		e.GET(fmt.Sprintf("%s/*", uiPrefix), uiHandler)
		logger.Info("Swagger UI configured apiPrefix", zap.String("ui_path", uiPrefix))

		return nil
	}
}

// Server configures and starts an Echo server with standard capabilities, and configuration functions.
func Server(serverConfig APIServerConfig, assetConfig assets.Config, templateGlobals pongo2.Context, configFns ...func(e *echo.Echo) error) error {
	logger := zap.L().With(zap.String("subsystem", "server"))

	e := echo.New()
	e.HideBanner = true
	e.Logger.SetOutput(io.Discard)

	// Configure main renderer to use pongo2
	webAssets := lo.Must(fs.Sub(assets.Assets(), "web"))
	webTemplateSet := pongo2.NewSet("web", pongo2.NewFSLoader(webAssets))
	webTemplateSet.Debug = assetConfig.DebugTemplates
	webTemplateSet.Globals = templateGlobals
	e.Renderer = pongorenderer.NewRenderer(webTemplateSet)

	// Setup Prometheus monitoring
	p := prometheus.NewPrometheus(version.Name, nil)
	p.Use(e)

	// Setup logging
	e.Use(echozap.ZapLogger(zap.L()))

	// Add ready and liveness endpoints
	e.GET("/-/ready", Ready)
	e.GET("/-/live", Live)
	e.GET("/-/started", Started)

	// Add static hosting endpoints
	e.GET("/", Index)

	e.GET("/css/*", StaticGet(webAssets, "text/css"))
	e.HEAD("/css/*", StaticHead(webAssets, "text/css"))

	e.GET("/js/*", StaticGet(webAssets, "application/javascript"))
	e.HEAD("/js/*", StaticHead(webAssets, "application/javascript"))

	//	e.GET("/css/*", Static(webAssets, "text/css"))
	//e.GET("/js/*", Static(webAssets, "application/javascript"))

	for _, configFn := range configFns {
		if err := configFn(e); err != nil {
			logger.Error("Failed calling configuration function", zap.Error(err))
		}
	}

	err := e.Start(fmt.Sprintf("%s:%d", serverConfig.Host, serverConfig.Port))
	return errors.Wrap(err, "Server")
}
