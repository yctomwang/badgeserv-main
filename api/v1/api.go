package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/flosch/pongo2/v6"
	"github.com/go-resty/resty/v2"
	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/tdewolff/minify"
	"github.com/tdewolff/minify/svg"
	"github.com/yctomwang/badgeserv/pkg/badges"
	"github.com/yctomwang/badgeserv/pkg/server/badgeconfig"
	"github.com/yctomwang/badgeserv/version"
	"go.withmatt.com/httpheaders"
)

//go:generate bash -c "oapi-codegen -package api openapi.yaml > api.gen.go"

var (
	ErrPredefinedBadgeNotFound = errors.New("Predefined badge name not found")
)

// ApiImpl implements the actual nmap-api.
type apiImpl struct {
	version          string
	badgeService     badges.BadgeService
	minify           *minify.M
	httpClient       *resty.Client
	predefinedBadges *badgeconfig.Config
	logger           *zap.Logger
}

const DynamicBadgeResponseName = "r"

func (a *apiImpl) generateETag(in []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(in))
}

func (a *apiImpl) GetBadgeDynamic(ctx echo.Context, params GetBadgeDynamicParams) error {
	//this params will contain things from the predefined badge if we were to go ahead with the predefined badge
	target := params.Target

	a.logger.Debug("Making outbound request", zap.String("target", target))
	resp, err := a.httpClient.NewRequest().Get(target)
	if err != nil {
		a.logger.Debug("Outbound HTTP request failed", zap.Error(err))
		return ctx.JSON(http.StatusBadGateway, &ClientError{
			Description: "Target HTTP request failed",
			Error:       err.Error(),
		})
	}
	// Check response status code
	// Check response status code
	statusCode := resp.StatusCode()
	if statusCode >= 200 && statusCode <= 299 {
		// If status code is 20x, set color to green and message to "OK"
		green := "green"
		ok := "OK"
		params.Color = &green
		params.Message = &ok
	} else {
		// If status code is not 20x, set color to red and message to "Failed"
		red := "red"
		failed := "Failed"
		params.Color = &red
		params.Message = &failed
	}

	var responseData interface{}
	if err := json.Unmarshal(resp.Body(), &responseData); err != nil {
		return ctx.JSON(http.StatusBadGateway, &ClientError{
			Description: "Response could not be unmarshalled to JSON",
			Error:       err.Error(),
		})
	}

	templateCtx := map[string]interface{}{}
	templateCtx[DynamicBadgeResponseName] = responseData
	//here see,s ;ole we are pumping in data to make the badge
	return a.getBadge(ctx, GetBadgeStaticParams{
		Label:   params.Label,
		Message: params.Message,
		Color:   params.Color,
	}, templateCtx)
}

func (a *apiImpl) GetBadgePredefined(ctx echo.Context) error {
	//TODO implement me
	panic("implement me")
}

func (a *apiImpl) GetBadgePredefinedPredefinedName(ctx echo.Context, predefinedName string, params GetBadgePredefinedPredefinedNameParams) error {
	//here is where we acutally check the predefined yaml that is loaded into this map
	badgeDef, ok := a.predefinedBadges.PredefinedBadges[predefinedName]
	if !ok {
		return ctx.JSON(http.StatusNotFound, &ClientError{
			Description: "Predefined badge with given name does not exist",
			Error:       ErrPredefinedBadgeNotFound.Error(),
		})
	}

	targetTemplate, err := pongo2.FromBytes([]byte(badgeDef.Target))
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, &ClientError{
			Description: "Predefined badge target template failed to parse",
			Error:       err.Error(),
		})
	}

	// This code should handle things for us .. but it doesn't work with the current generator. So instead parse
	// from the URL directly.
	//queryParams := map[string]interface{}{}
	//if params.Params != nil {
	//	if params.Params.AdditionalProperties != nil {
	//		queryParams = params.Params.AdditionalProperties
	//	}
	//}
	queryParams := lo.MapEntries(ctx.Request().URL.Query(), func(queryParamName string, v []string) (string, interface{}) {
		value := ""
		if len(v) > 0 {
			value = v[0]
		}

		return queryParamName, value
	})

	target, err := targetTemplate.Execute(lo.PickByKeys(queryParams, lo.Keys(badgeDef.Parameters)))
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, &ClientError{
			Description: "Predefined badge target template failed to execute",
			Error:       err.Error(),
		})
	}

	return a.GetBadgeDynamic(ctx, GetBadgeDynamicParams{
		Target:  target,
		Label:   &badgeDef.Label,
		Message: &badgeDef.Message,
		Color:   &badgeDef.Color,
	})
}

func (a *apiImpl) GetBadgeStatic(ctx echo.Context, params GetBadgeStaticParams) error {
	return a.getBadge(ctx, params, nil)
}

func (a *apiImpl) parseTemplate(ctx echo.Context, paramName string, templateString string) (*pongo2.Template, error) {
	tmpl, err := pongo2.FromBytes([]byte(templateString))
	if err != nil {
		return nil, ctx.JSON(http.StatusBadRequest, &ClientError{
			Description: fmt.Sprintf("%s template is invalid", paramName),
			Error:       err.Error(),
		})
	}
	return tmpl, nil
}

func (a *apiImpl) executeTemplate(ctx echo.Context, paramName string, template *pongo2.Template, templateCtx pongo2.Context) (string, error) {
	// Execute the templates
	result, err := template.Execute(templateCtx)
	if err != nil {
		return "", ctx.JSON(http.StatusBadRequest, &ClientError{
			Description: fmt.Sprintf("%s template execution failed", paramName),
			Error:       err.Error(),
		})
	}
	return result, nil
}

func (a *apiImpl) getBadge(ctx echo.Context, params GetBadgeStaticParams, templateCtx pongo2.Context) error {
	/*
		params contain information about label{{r.brand}}, message{{r.title}}, color"red"
	*/
	if templateCtx == nil {
		templateCtx = map[string]interface{}{}
	}

	// Parse the incoming templates
	labelTmpl, err := a.parseTemplate(ctx, "Label", lo.FromPtr(params.Label))
	if err != nil {
		return err
	}
	messageTmpl, err := a.parseTemplate(ctx, "Message", lo.FromPtr(params.Message))
	if err != nil {
		return err
	}
	colorTmpl, err := a.parseTemplate(ctx, "Color", lo.FromPtr(params.Color))
	if err != nil {
		return err
	}

	// Execute the templates
	label, err := a.executeTemplate(ctx, "Label", labelTmpl, templateCtx)
	if err != nil {
		return err
	}
	message, err := a.executeTemplate(ctx, "Message", messageTmpl, templateCtx)
	if err != nil {
		return err
	}
	color, err := a.executeTemplate(ctx, "Color", colorTmpl, templateCtx)
	if err != nil {
		return err
	}

	// Create the badge
	badge, err := a.badgeService.CreateBadge(badges.BadgeDesc{Title: label, Text: message, Color: color})
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, &ClientError{
			Description: "Badge generation failed",
			Error:       err.Error(),
		})
	}

	// Do the SVG response
	return a.svgResponse(ctx, badge)
}

func (a *apiImpl) svgResponse(ctx echo.Context, svgData string) error {
	minifiedSvg, err := a.minify.Bytes("image/svg+xml", []byte(svgData))
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, &ClientError{
			Description: "Badge minification failed",
			Error:       err.Error(),
		})
	}

	ctx.Response().Header().Set(httpheaders.Etag, a.generateETag([]byte(svgData)))
	ctx.Response().Header().Set(httpheaders.CacheControl, "no-cache")
	return ctx.Blob(http.StatusOK, "image/svg+xml", minifiedSvg)
}

// Config provides the up-front configuration necessary to launch an API.
type Config struct {
	BadgeService     badges.BadgeService
	HTTPClient       *resty.Client
	PredefinedBadges *badgeconfig.Config
}

// NewAPI returns the API server instance and the version prefix.
func NewAPI(apiConfig *Config) (ServerInterface, string) {
	if apiConfig.BadgeService == nil {
		return nil, "err"
	}

	minifier := minify.New()
	minifier.AddFunc("image/svg+xml", svg.Minify)

	const apiVersion = "v1"

	return &apiImpl{
		version.Version,
		apiConfig.BadgeService,
		minifier,
		apiConfig.HTTPClient,
		apiConfig.PredefinedBadges,
		zap.L().With(zap.String("app_version", version.Version), zap.String("api_version", apiVersion)),
	}, apiVersion
}

// GetOpenapiYaml implements returning the openapi.yaml file.
func (a *apiImpl) GetOpenapiYaml(ctx echo.Context) error {
	header := ctx.Response().Header()
	header.Set(httpheaders.ContentDisposition, "inline; filename=\"openapi.yaml\"")
	return ctx.Blob(http.StatusOK, "application/yaml;text/plain", OpenAPISpec)
}

func (a *apiImpl) GetPing(ctx echo.Context) error {
	now := time.Now()
	status := PingResponseStatus("ok")
	return ctx.JSON(http.StatusOK, &PingResponse{
		RespondedAt: &now,
		Status:      &status,
		Version:     &a.version,
	})
}

func (a *apiImpl) GetBadgeDynamicJSON(ctx echo.Context) error {
	targetURL := ctx.QueryParam("target")

	// Call a function to process the target URL and determine color and message
	color, message, err := a.processTargetURL(targetURL)
	if err != nil {
		// Handle error: Could not process target URL or failed to determine color/message
		return ctx.JSON(http.StatusInternalServerError, &ClientError{
			Description: "Error processing target URL",
			Error:       err.Error(),
		})
	}

	// Create the response
	responseData := struct {
		Target  string `json:"target"`
		Color   string `json:"color"`
		Message string `json:"message"`
	}{
		Target:  targetURL,
		Color:   color,
		Message: message,
	}

	// Return the JSON response
	return ctx.JSON(http.StatusOK, responseData)
}

//
func (a *apiImpl) processTargetURL(targetURL string) (color string, message string, err error) {
	// Make HTTP request to the targetURL
	resp, err := a.httpClient.NewRequest().Get(targetURL)
	if err != nil {
		return "", "", err
	}
	statusCode := resp.StatusCode()
	if statusCode >= 200 && statusCode <= 299 {
		color = "green"
		message = "OK"
	} else {
		color = "red"
		message = "Failed"
	}

	return color, message, nil
}

//todo: add a json endpoint for the predefined badges

func (a *apiImpl) GetPreDefinedJson(ctx echo.Context) error {
	targetURL := ctx.QueryParam("target")

	// Call a function to process the target URL and determine color and message
	color, message, err := a.processTargetURL(targetURL)
	if err != nil {
		// Handle error: Could not process target URL or failed to determine color/message
		return ctx.JSON(http.StatusInternalServerError, &ClientError{
			Description: "Error processing target URL",
			Error:       err.Error(),
		})
	}

	// Create the response
	responseData := struct {
		Target  string `json:"target"`
		Color   string `json:"color"`
		Message string `json:"message"`
	}{
		Target:  targetURL,
		Color:   color,
		Message: message,
	}

	// Return the JSON response
	return ctx.JSON(http.StatusOK, responseData)
}

func (a *apiImpl) GetBadgePredefinedPredefinedNameJSON(ctx echo.Context, predefinedName string) error {
	badgeDef, ok := a.predefinedBadges.PredefinedBadges[predefinedName]
	if !ok {
		return ctx.JSON(http.StatusNotFound, &ClientError{
			Description: "Predefined badge not found",
			Error:       "Badge not found",
		})
	}

	// Fetch data from the downstream service
	downstreamData, err := a.fetchDataFromDownstream(badgeDef.Target)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, &ClientError{
			Description: "Error fetching data from downstream",
			Error:       err.Error(),
		})
	}

	// Prepare data for template execution
	templateData := map[string]interface{}{
		"r": downstreamData, // Assuming the placeholders are like {{ r.someKey }}
	}

	// Replace placeholders in the badge template with actual values
	label, err := replacePlaceholders(badgeDef.Label, templateData)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, &ClientError{
			Description: "Error processing label template",
			Error:       err.Error(),
		})
	}
	message, err := replacePlaceholders(badgeDef.Message, templateData)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, &ClientError{
			Description: "Error processing message template",
			Error:       err.Error(),
		})
	}

	responseData := struct {
		Label   string `json:"label"`
		Message string `json:"message"`
		Color   string `json:"color"`
	}{
		Label:   label,
		Message: message,
		Color:   badgeDef.Color,
	}

	return ctx.JSON(http.StatusOK, responseData)
}

func replacePlaceholders(template string, data map[string]interface{}) (string, error) {
	t, err := pongo2.FromString(template)
	if err != nil {
		return "", err
	}
	output, err := t.Execute(pongo2.Context(data))
	if err != nil {
		return "", err
	}
	return output, nil
}

func (a *apiImpl) fetchDataFromDownstream(targetURL string) (map[string]interface{}, error) {
	resp, err := a.httpClient.NewRequest().Get(targetURL)
	if err != nil {
		return nil, err
	}

	var responseData map[string]interface{}
	if err := json.Unmarshal(resp.Body(), &responseData); err != nil {
		return nil, err
	}

	return responseData, nil
}
