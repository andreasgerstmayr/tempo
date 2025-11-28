package frontend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"text/template"
	"time"

	"github.com/go-kit/log/level"
	"github.com/gorilla/mux"
	"github.com/grafana/tempo/modules/frontend/docs"
	"github.com/grafana/tempo/pkg/api"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/traceql"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// add a mcp calls metric counter
var metricMCPToolCalls = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "tempo",
	Name:      "query_frontend_mcp_calls_total",
	Help:      "Total number of MCP calls",
}, []string{"tool"})

const (
	MetaTypeDocumentation   = "documentation"
	MetaTypeSearchResults   = "search-results"
	MetaTypeMetricsRange    = "metrics-range"
	MetaTypeMetricsInstant  = "metrics-instant"
	MetaTypeTrace           = "trace"
	MetaTypeAttributeNames  = "attribute-names"
	MetaTypeAttributeValues = "attribute-values"
	MetaTypeInstances       = "instances"
	MetaTypeTenants         = "tenants"
)

// handleListInstances returns a list of all configured Tempo instances
func (s *MCPServer) handleListInstances(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricMCPToolCalls.WithLabelValues(toolListInstances).Inc()

	level.Info(s.logger).Log("msg", "listing Tempo instances")

	instances := make([]string, len(s.instances))
	for i := range s.instances {
		instances[i] = s.instances[i].Name
	}

	body, err := json.Marshal(instances)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to marshal instances: %v", err)), nil
	}

	return toolResult(string(body), MetaTypeInstances, "json", "1"), nil
}

// handleListTenants returns a list of tenants for a given instance
func (s *MCPServer) handleListTenants(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricMCPToolCalls.WithLabelValues(toolListTenants).Inc()

	instanceName, err := request.RequireString("instance")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if instanceName == "" {
		return mcp.NewToolResultError("instance name must not be empty"), nil
	}

	level.Info(s.logger).Log("msg", "listing tenants", "instance", instanceName)

	var instance *MCPInstanceConfig
	for i := range s.instances {
		if s.instances[i].Name == instanceName {
			instance = &s.instances[i]
			break
		}
	}

	if instance == nil {
		return mcp.NewToolResultError(fmt.Sprintf("instance '%s' not found", instanceName)), nil
	}

	if len(instance.Tenants) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("instance '%s' is a single-tenant Tempo instance", instanceName)), nil
	}

	body, err := json.Marshal(instance.Tenants)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to marshal tenants: %v", err)), nil
	}

	return toolResult(string(body), MetaTypeTenants, "json", "1"), nil
}

func (s *MCPServer) handleTraceQLDocs(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricMCPToolCalls.WithLabelValues(toolDocsTraceQL).Inc()

	docType, err := request.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	level.Info(s.logger).Log("msg", "traceql docs requested", "doc_type", docType)

	// Get the appropriate documentation content based on the requested type
	content := docs.GetDocsContent(docType)

	return toolResult(content, MetaTypeDocumentation, "markdown", "1"), nil
}

// handleSearch handles the traceql-search tool
func (s *MCPServer) handleSearch(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricMCPToolCalls.WithLabelValues(toolTraceQLSearch).Inc()

	query, err := request.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var startEpoch, endEpoch int64

	start := request.GetString("start", "")
	end := request.GetString("end", "")

	level.Info(s.logger).Log("msg", "searching traces", "query", query, "start", start, "end", end)

	if start == "" {
		startEpoch = time.Now().Add(-1 * time.Hour).Unix()
	} else {
		startTS, err := time.Parse(time.RFC3339, start)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid start time: %v", err)), nil
		}
		startEpoch = startTS.Unix()
	}
	if end == "" {
		endEpoch = time.Now().Unix()
	} else {
		endTS, err := time.Parse(time.RFC3339, end)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid end time: %v", err)), nil
		}
		endEpoch = endTS.Unix()
	}

	parsed, err := traceql.Parse(query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query parse error. Consult TraceQL docs tools: %v", err)), nil
	}

	if parsed.MetricsPipeline != nil || parsed.MetricsSecondStage != nil {
		return mcp.NewToolResultError("TraceQL metrics query received on traceql-search tool. Use the traceql-metrics-instant or traceql-metrics-range tool instead"), nil
	}

	searchReq := &tempopb.SearchRequest{
		Query: query,
		Start: uint32(startEpoch),
		End:   uint32(endEpoch),
	}

	req, err := api.BuildSearchRequest(nil, searchReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to build search request: %v", err)), nil
	}
	req.URL.Path = s.buildPath(api.PathSearch)

	body, err := s.handleHTTP(ctx, s.frontend.SearchHandler, request, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return toolResult(body, MetaTypeSearchResults, "json", "1"), nil
}

// handleInstantQuery handles the traceql-metrics-instant tool
func (s *MCPServer) handleInstantQuery(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricMCPToolCalls.WithLabelValues(toolTraceQLMetricsInstant).Inc()

	query, err := request.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var startEpochNanos, endEpochNanos int64

	start := request.GetString("start", "")
	end := request.GetString("end", "")

	level.Info(s.logger).Log("msg", "executing instant metrics query", "query", query, "start", start, "end", end)

	if start == "" {
		startEpochNanos = time.Now().Add(-1 * time.Hour).UnixNano()
	} else {
		startTS, err := time.Parse(time.RFC3339, start)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid start time: %v", err)), nil
		}
		startEpochNanos = startTS.UnixNano()
	}
	if end == "" {
		endEpochNanos = time.Now().UnixNano()
	} else {
		endTS, err := time.Parse(time.RFC3339, end)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid end time: %v", err)), nil
		}
		endEpochNanos = endTS.UnixNano()
	}

	parsed, err := traceql.Parse(query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query parse error. Consult TraceQL docs tools: %v", err)), nil
	}

	if parsed.MetricsPipeline == nil {
		return mcp.NewToolResultError("TraceQL search query received on instant query tool. Use the traceql-search tool instead"), nil
	}

	queryInstantReq := &tempopb.QueryInstantRequest{
		Query: query,
		Start: uint64(startEpochNanos),
		End:   uint64(endEpochNanos),
	}

	req := api.BuildQueryInstantRequest(nil, queryInstantReq)
	req.URL.Path = s.buildPath(api.PathMetricsQueryInstant)

	body, err := s.handleHTTP(ctx, s.frontend.MetricsQueryInstantHandler, request, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return toolResult(body, MetaTypeMetricsInstant, "json", "1"), nil
}

func (s *MCPServer) handleRangeQuery(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricMCPToolCalls.WithLabelValues(toolTraceQLMetricsRange).Inc()

	query, err := request.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var startEpochNanos, endEpochNanos int64

	start := request.GetString("start", "")
	end := request.GetString("end", "")

	level.Info(s.logger).Log("msg", "executing range metrics query", "query", query, "start", start, "end", end)

	if start == "" {
		startEpochNanos = time.Now().Add(-1 * time.Hour).UnixNano()
	} else {
		startTS, err := time.Parse(time.RFC3339, start)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid start time: %v", err)), nil
		}
		startEpochNanos = startTS.UnixNano()
	}
	if end == "" {
		endEpochNanos = time.Now().UnixNano()
	} else {
		endTS, err := time.Parse(time.RFC3339, end)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid end time: %v", err)), nil
		}
		endEpochNanos = endTS.UnixNano()
	}

	parsed, err := traceql.Parse(query)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("query parse error. Consult TraceQL docs tools: %v", err)), nil
	}

	if parsed.MetricsPipeline == nil {
		return mcp.NewToolResultError("TraceQL search query received on range query tool. Use the traceql-search tool instead"), nil
	}

	queryRangeReq := &tempopb.QueryRangeRequest{
		Query: query,
		Start: uint64(startEpochNanos),
		End:   uint64(endEpochNanos),
	}

	req := api.BuildQueryRangeRequest(nil, queryRangeReq, "")
	req.URL.Path = s.buildPath(api.PathMetricsQueryRange)

	body, err := s.handleHTTP(ctx, s.frontend.MetricsQueryRangeHandler, request, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return toolResult(body, MetaTypeMetricsRange, "json", "1"), nil
}

// handleGetTrace handles the get-trace tool
func (s *MCPServer) handleGetTrace(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricMCPToolCalls.WithLabelValues(toolGetTrace).Inc()

	traceID, err := request.RequireString("trace_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	level.Info(s.logger).Log("msg", "getting trace", "trace_id", traceID)

	httpReq := &http.Request{
		Method: "GET",
		URL:    &url.URL{Path: s.buildPath("/api/v2/traces/" + url.PathEscape(traceID))},
	}
	httpReq, ctx = injectMuxVars(ctx, httpReq, map[string]string{"traceID": traceID})

	body, err := s.handleHTTP(ctx, s.frontend.TraceByIDHandlerV2, request, httpReq)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return toolResult(body, MetaTypeTrace, "json", "2"), nil
}

// handleGetAttributeNames handles the get-attribute-names tool
func (s *MCPServer) handleGetAttributeNames(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricMCPToolCalls.WithLabelValues(toolGetAttributeNames).Inc()

	level.Info(s.logger).Log("msg", "getting attribute names")

	searchTagsReq := &tempopb.SearchTagsRequest{
		Scope: request.GetString("scope", ""),
	}

	req, err := api.BuildSearchTagsRequest(nil, searchTagsReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to build search request: %v", err)), nil
	}
	req.URL.Path = s.buildPath(api.PathSearchTagsV2)

	body, err := s.handleHTTP(ctx, s.frontend.SearchTagsV2Handler, request, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return toolResult(body, MetaTypeAttributeNames, "json", "2"), nil
}

// handleGetAttributeValues handles the get-attribute-values tool
func (s *MCPServer) handleGetAttributeValues(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	metricMCPToolCalls.WithLabelValues(toolGetAttributeValues).Inc()

	name, err := request.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	query := request.GetString("filter-query", "")
	if query != "" {
		q := traceql.ExtractMatchers(query)
		if traceql.IsEmptyQuery(q) {
			return mcp.NewToolResultError("filter-query invalid. It can only have one spanset and only &&'ed conditions like { <cond> && <cond> && ... }"), nil
		}
	}

	level.Info(s.logger).Log("msg", "getting attribute values", "name", name, "filter query", query)

	searchTagValuesReq := &tempopb.SearchTagValuesRequest{
		TagName: name,
		Query:   query,
	}

	req, err := api.BuildSearchTagValuesRequest(nil, searchTagValuesReq)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to build search request: %v", err)), nil
	}
	req.URL.Path = s.buildPath("/api/v2/search/tag/" + url.PathEscape(name) + "/values")

	req, ctx = injectMuxVars(ctx, req, map[string]string{api.MuxVarTagName: name})

	body, err := s.handleHTTP(ctx, s.frontend.SearchTagsValuesV2Handler, request, req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return toolResult(body, MetaTypeAttributeValues, "json", "2"), nil
}

func (s *MCPServer) handleHTTP(ctx context.Context, handler http.Handler, request mcp.CallToolRequest, httpReq *http.Request) (string, error) {
	isInstanceAware := len(s.instances) > 0

	if isInstanceAware {
		return s.handleRemoteHTTP(ctx, request, httpReq)
	} else {
		return handleLocalHTTP(ctx, handler, httpReq)
	}
}

func handleLocalHTTP(ctx context.Context, handler http.Handler, req *http.Request) (string, error) {
	rw := newResponseBuffer()
	req = req.WithContext(ctx)

	if req.Body == nil {
		req.Body = io.NopCloser(bytes.NewReader([]byte{})) // prevents panic
	}

	if req.Header == nil {
		req.Header = make(http.Header)
	}

	// tell the query frontend we want content formatted for an LLM
	req.Header.Set(api.HeaderAccept, api.HeaderAcceptLLM)

	if req.RequestURI == "" {
		req.RequestURI = req.URL.RequestURI()
	}

	handler.ServeHTTP(rw, req)

	body := rw.body.String()

	if rw.status != http.StatusOK {
		return "", fmt.Errorf("tool failed with http status code %d and reason %s", rw.status, body)
	}

	return body, nil
}

func (s *MCPServer) handleRemoteHTTP(ctx context.Context, request mcp.CallToolRequest, httpReq *http.Request) (string, error) {
	instanceName, err := request.RequireString("instance")
	if err != nil {
		return "", err
	}
	if instanceName == "" {
		return "", errors.New("instance name must not be empty")
	}

	var instance *MCPInstanceConfig
	for i := range s.instances {
		if s.instances[i].Name == instanceName {
			instance = &s.instances[i]
			break
		}
	}

	if instance == nil {
		return "", fmt.Errorf("instance '%s' not found", instanceName)
	}

	var tenantName string
	if len(instance.Tenants) > 0 {
		tenantName, err = request.RequireString("tenant")
		if err != nil {
			return "", err
		}
		if tenantName == "" {
			return "", errors.New("tenant name must not be empty")
		}
	}

	httpReq = httpReq.WithContext(ctx)
	if httpReq.Header == nil {
		httpReq.Header = make(http.Header)
	}

	// tell the query frontend we want content formatted for an LLM
	httpReq.Header.Set(api.HeaderAccept, api.HeaderAcceptLLM)

	endpoint, err := renderEndpointTemplate(instance.Endpoint, tenantName)
	if err != nil {
		return "", err
	}

	url, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	httpReq.URL.Scheme = url.Scheme
	httpReq.URL.Host = url.Host
	httpReq.URL.Path = url.Path + httpReq.URL.Path

	level.Info(s.logger).Log("msg", "forwarding request", "instance", instanceName, "tenant", tenantName, "url", httpReq.URL)

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tool failed with http status code %d and reason %s", resp.StatusCode, body)
	}

	return string(body), nil
}

// injectMuxVars uses the mux.SetVars method to add vars into the context that can be used by downstream handlers.
// a few Tempo endpoints rely on the mux routing package extracting vars from the request path. this method allows
// us to do the same for MCP tools.
func injectMuxVars(ctx context.Context, req *http.Request, vars map[string]string) (*http.Request, context.Context) {
	req = req.WithContext(ctx)
	req = mux.SetURLVars(req, vars)

	return req, req.Context()
}

// buildPath is a helper method to build a path with the path prefix
func (s *MCPServer) buildPath(p string) string {
	return path.Join(s.pathPrefix, p)
}

func renderEndpointTemplate(endpoint string, tenant string) (string, error) {
	templ, err := template.New("endpoint").Parse(endpoint)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	err = templ.Execute(&buf, map[string]interface{}{
		"Tenant": tenant,
	})
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

func toolResult(body string, contentType string, encoding string, version string) *mcp.CallToolResult {
	res := mcp.NewToolResultText(body)
	res.Meta = &mcp.Meta{AdditionalFields: map[string]any{
		"type":     contentType,
		"encoding": encoding,
		"version":  version,
	}}

	return res
}

// responseBuffer
type responseBuffer struct {
	status      int
	header      http.Header
	body        *bytes.Buffer
	wroteHeader bool
}

func newResponseBuffer() *responseBuffer {
	return &responseBuffer{
		status: http.StatusOK,
		header: http.Header{},
		body:   bytes.NewBuffer(nil),
	}
}

func (rb *responseBuffer) Header() http.Header {
	return rb.header
}

func (rb *responseBuffer) WriteHeader(code int) {
	if rb.wroteHeader {
		return // Prevent multiple calls
	}
	rb.status = code
	rb.wroteHeader = true
}

func (rb *responseBuffer) Write(data []byte) (int, error) {
	if !rb.wroteHeader {
		rb.WriteHeader(http.StatusOK)
	}
	return rb.body.Write(data)
}
