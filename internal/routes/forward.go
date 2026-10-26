package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"relay-api/internal/config"
	"relay-api/internal/db"
	"relay-api/internal/httperr"
	"relay-api/internal/middleware"
	"relay-api/internal/util"
)

// ForwardRoutes serves the JSON forwarder mounted at /open/* (and, later, the
// same logic on /free with a different handler). It maps a request path to an
// API config, applies the config's templates and forwards the result.
type ForwardRoutes struct {
	cfg     config.Config
	secrets *util.SecretBox
	route   string
}

// NewForwardRoutes builds the forwarder for a mount ("open"/"free").
func NewForwardRoutes(cfg config.Config, secrets *util.SecretBox, route string) *ForwardRoutes {
	return &ForwardRoutes{cfg: cfg, secrets: secrets, route: route}
}

// Register mounts the catch-all handler under prefix.
func (h *ForwardRoutes) Register(mux *http.ServeMux, prefix string) {
	mux.HandleFunc(prefix+"/{path...}", h.handle)
}

func (h *ForwardRoutes) handle(w http.ResponseWriter, r *http.Request) {
	auth := middleware.APIAuthFrom(r.Context())
	requestID := middleware.RequestID(r.Context())
	startedAt := time.Now()

	configPath := strings.Trim(r.PathValue("path"), "/")
	apiConfig, rest, err := db.ResolveAPIConfig(configPath, h.route)
	if err != nil {
		internalError(w, "resolve api config", err)
		return
	}
	if apiConfig == nil {
		message := "API config '" + configPath + "' not found"
		if wildcard, _ := db.FindWildcardConfigBelow(configPath, h.route); wildcard != "" {
			message = "API config '" + configPath + "' not found - '" + wildcard +
				"' only matches paths below it, e.g. /" + h.route + "/" + configPath + "/<id>"
		}
		httperr.Write(w, http.StatusNotFound, message, "not_found_error", "config_not_found")
		return
	}

	// API-config permission (allowed_apis); empty = unrestricted.
	if auth != nil {
		allowed := parseStrings(auth.Key.AllowedAPIs)
		if len(allowed) > 0 && !containsString(allowed, apiConfig.Name) {
			httperr.Write(w, http.StatusForbidden, "API config '"+apiConfig.Name+"' is not allowed for this API key", "permission_error", "config_not_allowed")
			return
		}
	}

	var targetURL *string
	var upstreamHeaders map[string]string
	var headerSnapshots func() (*string, *string)
	if h.cfg.UsageHeaderSnapshots {
		headerSnapshots = func() (*string, *string) {
			return snapshotHTTPHeaders(r.Header), snapshotHeaderMap(upstreamHeaders)
		}
	}
	record := func(status int, errMsg *string) {
		var keyPtr *int64
		if auth != nil {
			keyID := auth.Key.ID
			keyPtr = &keyID
		}
		configID := apiConfig.ID
		latency := time.Since(startedAt).Milliseconds()
		db.RecordUsageAsync(db.UsageRecord{
			RequestID:       requestID,
			APIKeyID:        keyPtr,
			Kind:            "api",
			APIConfigID:     &configID,
			TargetURL:       targetURL,
			LatencyMs:       &latency,
			Status:          &status,
			Error:           errMsg,
			HeaderSnapshots: headerSnapshots,
		})
	}

	clientMethod := r.Method
	method := util.ResolveUpstreamMethod(apiConfig.Method, clientMethod)

	rawBody := middleware.RequestBody(r.Context())
	configHeaders := parseStringMap(apiConfig.Headers)
	requestTemplate := parseJSONValue(apiConfig.RequestTemplate)
	needsTemplateContext := requestTemplate != nil || hasTemplatePlaceholder(configHeaders)

	// The template context is only built when a template actually references
	// it. A plain passthrough request skips the JSON parse and the header/query
	// maps entirely.
	var templateContext map[string]any
	if needsTemplateContext {
		var clientBody any
		if clientMethod != http.MethodGet && clientMethod != http.MethodHead && len(rawBody) > 0 {
			var parsed any
			if json.Unmarshal(rawBody, &parsed) == nil {
				clientBody = parsed
			} else {
				clientBody = string(rawBody)
			}
		}
		// Template contexts are plain map[string]any so {{headers.x}} /
		// {{query.x}} lookups work (templateLookup only descends into
		// JSON-shaped maps).
		headersContext := map[string]any{}
		for key, values := range r.Header {
			if len(values) > 0 {
				headersContext[strings.ToLower(key)] = values[0]
			}
		}
		queryContext := map[string]any{}
		for key, values := range r.URL.Query() {
			if len(values) > 0 {
				queryContext[key] = values[0]
			}
		}
		templateContext = map[string]any{"body": clientBody, "headers": headersContext, "query": queryContext}
	}

	renderedConfigHeaders := map[string]string{}
	for key, value := range configHeaders {
		if strings.Contains(value, "{{") {
			renderedConfigHeaders[key] = asString(util.RenderTemplate(value, templateContext))
		} else {
			renderedConfigHeaders[key] = value
		}
	}
	upstreamHeaders = util.ForwardedRequestHeaders(r.Header)
	for key, value := range renderedConfigHeaders {
		upstreamHeaders[key] = value
	}
	upstreamHeaders["x-request-id"] = requestID

	if apiConfig.APIKey != nil && *apiConfig.APIKey != "" {
		plaintext, err := h.secrets.Decrypt(*apiConfig.APIKey)
		if err != nil {
			internalError(w, "decrypt api config key", err)
			return
		}
		if plaintext != "" && !hasHeader(renderedConfigHeaders, "authorization") {
			upstreamHeaders["authorization"] = "Bearer " + plaintext
		}
	}

	parsedURL, err := util.ValidateUpstreamURL(apiConfig.URL)
	if err != nil {
		invalidRequest(w, err.Error())
		return
	}
	if rest != "" {
		parsedURL.Path = strings.TrimRight(parsedURL.Path, "/") + "/" + rest
	}
	parsedURL.RawQuery = r.URL.RawQuery
	target := parsedURL.Scheme + "://" + parsedURL.Host + parsedURL.Path
	targetURL = &target

	var requestBody io.Reader
	if requestTemplate != nil {
		upstreamBody := util.RenderTemplate(requestTemplate, templateContext)
		if unresolved := util.FindUnresolved(requestTemplate, templateContext); len(unresolved) > 0 {
			message := "Request template has unresolved placeholders: " + strings.Join(unresolved, ", ")
			invalidRequest(w, message)
			record(http.StatusBadRequest, &message)
			return
		}
		if method != http.MethodGet && method != http.MethodHead && upstreamBody != nil {
			reader, err := requestBodyReader(upstreamBody)
			if err != nil {
				internalError(w, "marshal upstream body", err)
				return
			}
			requestBody = reader
		}
	} else if method != http.MethodGet && method != http.MethodHead &&
		clientMethod != http.MethodGet && clientMethod != http.MethodHead && len(rawBody) > 0 {
		// Pure passthrough: reuse the bytes BodyParser already buffered rather
		// than decoding and re-encoding the JSON body.
		requestBody = bytes.NewReader(rawBody)
	}

	timeoutMs := apiConfig.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = h.cfg.RequestTimeoutMs
	}

	response, err := util.FetchUpstream(r.Context(), parsedURL.String(), util.FetchOptions{
		Method:    method,
		Headers:   upstreamHeaders,
		Body:      requestBody,
		TimeoutMs: timeoutMs,
	})
	if err != nil {
		message := err.Error()
		if errors.Is(err, util.ErrUpstreamTimeout) {
			httperr.Write(w, http.StatusGatewayTimeout, "Upstream request timed out", "upstream_error", "upstream_timeout")
			record(http.StatusGatewayTimeout, &message)
			return
		}
		httperr.Write(w, http.StatusBadGateway, message, "upstream_error", "upstream_error")
		record(http.StatusBadGateway, &message)
		return
	}
	defer response.Body.Close()

	responseTemplate := parseJSONValue(apiConfig.ResponseTemplate)
	if responseTemplate != nil && response.StatusCode < 300 {
		data, err := io.ReadAll(response.Body)
		if err != nil {
			message := err.Error()
			httperr.Write(w, http.StatusBadGateway, message, "upstream_error", "upstream_error")
			record(http.StatusBadGateway, &message)
			return
		}
		var upstreamJSON any
		if err := json.Unmarshal(data, &upstreamJSON); err != nil {
			httperr.Write(w, http.StatusBadGateway, "Upstream returned non-JSON response but response template is configured", "upstream_error", "upstream_error")
			record(http.StatusBadGateway, stringPtr("Response template configured but upstream returned non-JSON"))
			return
		}
		bodyContext := map[string]any{"data": upstreamJSON}
		rendered := util.RenderTemplate(responseTemplate, bodyContext)
		if unresolved := util.FindUnresolved(responseTemplate, bodyContext); len(unresolved) > 0 {
			message := "Response template has unresolved placeholders: " + strings.Join(unresolved, ", ")
			httperr.Write(w, http.StatusBadGateway, message, "upstream_error", "upstream_error")
			record(http.StatusBadGateway, &message)
			return
		}
		encoded, err := json.Marshal(rendered)
		if err != nil {
			internalError(w, "marshal rendered response", err)
			return
		}
		writeUpstreamHeaders(w, response.Header, requestID)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(encoded)
		record(response.StatusCode, nil)
		return
	}

	// Pure passthrough.
	writeUpstreamHeaders(w, response.Header, requestID)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
	record(response.StatusCode, nil)
}

func writeUpstreamHeaders(w http.ResponseWriter, header http.Header, requestID string) {
	for key, value := range util.FilterResponseHeaders(header) {
		w.Header().Set(key, value)
	}
	w.Header().Set("X-Request-ID", requestID)
}

// requestBodyReader turns a rendered template result into an upstream reader.
func requestBodyReader(value any) (io.Reader, error) {
	if text, ok := value.(string); ok {
		return strings.NewReader(text), nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(encoded), nil
}

// hasTemplatePlaceholder reports whether any configured header value needs the
// template engine.
func hasTemplatePlaceholder(values map[string]string) bool {
	for _, value := range values {
		if strings.Contains(value, "{{") {
			return true
		}
	}
	return false
}

// parseStringMap decodes a stored JSON object of strings.
func parseStringMap(raw *string) map[string]string {
	if raw == nil || *raw == "" {
		return nil
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(*raw), &values); err != nil {
		return nil
	}
	return values
}

// parseJSONValue decodes a stored JSON value (nil when absent or invalid).
func parseJSONValue(raw *string) any {
	if raw == nil || *raw == "" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(*raw), &value); err != nil {
		return nil
	}
	return value
}

func parseStrings(raw *string) []string {
	if raw == nil || *raw == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(*raw), &values); err != nil {
		return nil
	}
	return values
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// asString renders a template result as a header value.
func asString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

func hasHeader(headers map[string]string, name string) bool {
	for key := range headers {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

func stringPtr(value string) *string {
	return &value
}
