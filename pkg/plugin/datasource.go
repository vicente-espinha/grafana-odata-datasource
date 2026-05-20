package plugin

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/d-velop/grafana-odata-datasource/pkg/plugin/odata"
	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/backend/datasource"
	"github.com/grafana/grafana-plugin-sdk-go/backend/httpclient"
	"github.com/grafana/grafana-plugin-sdk-go/backend/instancemgmt"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

var (
	_ backend.QueryDataHandler    = (*ODataSource)(nil)
	_ backend.CheckHealthHandler  = (*ODataSource)(nil)
	_ backend.CallResourceHandler = (*ODataSource)(nil)
)

type ODataSource struct {
	im instancemgmt.InstanceManager
}

type DatasourceSettings struct {
	URLSpaceEncoding string `json:"urlSpaceEncoding"`
}

func newDatasourceInstance(ctx context.Context, settings backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	clientOptions, err := settings.HTTPClientOptions(ctx)
	if err != nil {
		return nil, err
	}
	client, err := httpclient.New(clientOptions)
	if err != nil {
		return nil, err
	}

	var dsSettings DatasourceSettings
	if settings.JSONData != nil && len(settings.JSONData) > 1 {
		if err := json.Unmarshal(settings.JSONData, &dsSettings); err != nil {
			return nil, err
		}
	}

	return &ODataSourceInstance{
		client: &ODataClientImpl{
			httpClient:       client,
			baseUrl:          settings.URL,
			urlSpaceEncoding: dsSettings.URLSpaceEncoding,
			cookieHeader:     "",
		},
	}, nil
}

type ODataSourceInstance struct {
	client ODataClient
}

func NewODataSource(ctx context.Context, _ backend.DataSourceInstanceSettings) (instancemgmt.Instance, error) {
	im := datasource.NewInstanceManager(newDatasourceInstance)
	ds := &ODataSource{
		im: im,
	}
	return ds, nil
}

func (ds *ODataSource) getClientInstance(ctx context.Context, pluginContext backend.PluginContext) ODataClient {
	instance, _ := ds.im.Get(ctx, pluginContext)
	clientInstance := instance.(*ODataSourceInstance).client
	return clientInstance
}

func (ds *ODataSource) QueryData(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	rawInstance, _ := ds.im.Get(ctx, req.PluginContext)
	dsInstance := rawInstance.(*ODataSourceInstance)

	clientImpl, ok := dsInstance.client.(*ODataClientImpl)
	if !ok {
		return nil, fmt.Errorf("expected *ODataClientImpl, got something else")
	}

	cookieHeaders, ok := req.Headers["Cookie"]
	if !ok || len(cookieHeaders) == 0 {
		clientImpl.SetCookieHeader("")
	} else {
		clientImpl.SetCookieHeader(cookieHeaders)
	}
	
	clientInstance := ds.getClientInstance(ctx, req.PluginContext)
	response := backend.NewQueryDataResponse()
	for _, q := range req.Queries {
		res := ds.query(clientInstance, q)
		response.Responses[q.RefID] = res
	}
	return response, nil
}

func (ds *ODataSource) CheckHealth(ctx context.Context, req *backend.CheckHealthRequest) (*backend.CheckHealthResult, error) {
	var status backend.HealthStatus
	var message string
	clientInstance := ds.getClientInstance(ctx, req.PluginContext)
	var res, err = clientInstance.GetServiceRoot()
	if err != nil {
		status = backend.HealthStatusError
		message = fmt.Sprintf("Health check failed: %s", err.Error())
	} else {
		if res.StatusCode == 200 {
			status = backend.HealthStatusOk
			message = "Data Source is working as expected."
		} else {
			status = backend.HealthStatusError
			message = fmt.Sprintf("Health check failed, datasource exists but given path does not. "+
				"Statuscode: %d", res.StatusCode)
		}
	}
	return &backend.CheckHealthResult{
		Status:  status,
		Message: message,
	}, nil
}

func (ds *ODataSource) CallResource(ctx context.Context, req *backend.CallResourceRequest,
	sender backend.CallResourceResponseSender) error {
	rawInstance, _ := ds.im.Get(ctx, req.PluginContext)
	dsInstance := rawInstance.(*ODataSourceInstance)

	clientImpl, ok := dsInstance.client.(*ODataClientImpl)
	if !ok {
		return fmt.Errorf("expected *ODataClientImpl, got something else")
	}

	cookieHeaders, ok := req.Headers["Cookie"]
	if !ok || len(cookieHeaders) == 0 {
		clientImpl.SetCookieHeader("")
	} else {
		combined := strings.Join(cookieHeaders, "; ")
		clientImpl.SetCookieHeader(combined)
	}

	switch req.Path {
	case "metadata":
		return ds.getMetadata(ctx, req, sender)
	default:
		return sender.Send(&backend.CallResourceResponse{
			Status: http.StatusNotFound,
		})
	}
}

func (ds *ODataSource) query(clientInstance ODataClient, query backend.DataQuery) backend.DataResponse {
	log.DefaultLogger.Debug("query", "query.JSON", string(query.JSON))
	
	var response backend.DataResponse
	var qm queryModel
	if err := json.Unmarshal(query.JSON, &qm); err != nil {
		return errorResponse("error unmarshalling query json", err)
	}

	if isQueryEmpty(qm) {
		return response
	}

	frame := ds.initFrame(query.RefID)

	props := ds.prepareProperties(qm)
	resp, err := clientInstance.Get(qm.ODataQueryString, qm.EntitySet.Name, props,
		append(qm.FilterConditions, TimeRangeToFilter(query.TimeRange, qm.TimeProperty)...), qm.UsePost)
	if err != nil {
		log.DefaultLogger.Error("Get() failed", "error", err, "queryString", qm.ODataQueryString, "usePost", qm.UsePost)
		return errorResponse("odata get failed", err)
	}
	defer resp.Body.Close()

	log.DefaultLogger.Error("Get() response", "status", resp.Status, "statusCode", resp.StatusCode, 
		"queryString", qm.ODataQueryString, "usePost", qm.UsePost, "entitySet", qm.EntitySet.Name)
	if resp.StatusCode != http.StatusOK {
		return errorResponse(fmt.Sprintf("get failed with status code %d", resp.StatusCode), nil)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return errorResponse("reading response body failed", err)
	}

	log.DefaultLogger.Debug("Get() response body", "body", string(bodyBytes), "bodyLength", len(bodyBytes))

	var result odata.Response
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return errorResponse("unmarshalling response body failed", err)
	}

	log.DefaultLogger.Debug("query complete", "noOfEntities", len(result.Value))

	entityProperties, err := ds.resolveProperties(clientInstance, qm, props)
	if err != nil {
		return errorResponse("resolving entity properties failed", err)
	}

	if len(result.Value) > 0 {
		ds.populateFields(frame, result.Value[0], entityProperties, qm.ODataQueryString)
	}

	for _, entry := range result.Value {
		ds.appendRow(frame, entry, entityProperties)
	}

	response.Frames = append(response.Frames, frame)
	return response
}

func (ds *ODataSource) getMetadata(ctx context.Context, req *backend.CallResourceRequest, sender backend.CallResourceResponseSender) error {
	clientInstance := ds.getClientInstance(ctx, req.PluginContext)
	responseBody, err := ds.fetchMetadata(clientInstance)
	if err != nil {
		return err
	}

	return sender.Send(&backend.CallResourceResponse{
		Status: http.StatusOK,
		Body:   responseBody,
	})
}

func (ds *ODataSource) fetchMetadata(clientInstance ODataClient) ([]byte, error) {
	resp, err := clientInstance.GetMetadata()
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get metadata failed with status code %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.DefaultLogger.Error("error reading response body")
		return nil, err
	}

	var edmx odata.Edmx
	err = xml.Unmarshal(bodyBytes, &edmx)
	if err != nil {
		log.DefaultLogger.Error("error unmarshalling response body")
		return nil, err
	}

	metadata := schema{
		EntityTypes: make(map[string]entityType),
		EntitySets:  make(map[string]entitySet),
	}
	for _, ds := range edmx.DataServices {
		for _, s := range ds.Schemas {
			for _, et := range s.EntityTypes {
				qualifiedName := s.Namespace + "." + et.Name
				var properties []property
				for _, p := range et.Properties {
					prop := property{
						Name: p.Name,
						Type: p.Type,
					}
					properties = append(properties, prop)
				}
				metadata.EntityTypes[qualifiedName] = entityType{
					Name:          et.Name,
					QualifiedName: qualifiedName,
					Properties:    properties,
				}
			}
			for _, ec := range s.EntityContainers {
				for _, es := range ec.EntitySet {
					metadata.EntitySets[es.Name] = entitySet{
						Name:       es.Name,
						EntityType: es.EntityType,
					}
				}
			}
		}
	}

	responseBody, err := json.Marshal(metadata)
	if err != nil {
		log.DefaultLogger.Error("error marshalling response body")
		return nil, err
	}

	return responseBody, nil
}

func hasNonEmptyName(properties []property) bool {
    for _, prop := range properties {
        if prop.Name != "" {
            return true
        }
    }
    return false
}

func errorResponse(msg string, err error) backend.DataResponse {
	return backend.DataResponse{Error: fmt.Errorf("%s: %w", msg, err)}
}

func isQueryEmpty(qm queryModel) bool {
	return qm.ODataQueryString == "" && qm.TimeProperty == nil && 
		(len(qm.Properties) == 0 || !hasNonEmptyName(qm.Properties))
}

func (ds *ODataSource) initFrame(refID string) *data.Frame {
	frame := data.NewFrame("response")
	frame.Name = refID
	frame.Meta = &data.FrameMeta{PreferredVisualization: data.VisTypeTable}
	return frame
}

func (ds *ODataSource) prepareProperties(qm queryModel) []property {
	props := qm.Properties
	if qm.TimeProperty != nil {
		props = append(props, *qm.TimeProperty)
	}
	return props
}

func (ds *ODataSource) resolveProperties(clientInstance ODataClient, qm queryModel, defaultProps []property) ([]property, error) {
	if qm.ODataQueryString == "" {
		return defaultProps, nil
	}

	tableName := qm.ODataQueryString
	if index := strings.Index(qm.ODataQueryString, "?"); index != -1 {
		tableName = qm.ODataQueryString[:index]
	}

	metadataBytes, err := ds.fetchMetadata(clientInstance)
	if err != nil {
		return nil, err
	}

	var metadata schema
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return nil, err
	}

	entityType, ok := metadata.EntitySets[tableName]
	if !ok {
		return nil, fmt.Errorf("entity set %s not found in metadata", tableName)
	}

	return metadata.EntityTypes[entityType.EntityType].Properties, nil
}

func (ds *ODataSource) populateFields(frame *data.Frame, firstEntry map[string]interface{}, entityProps []property, queryStr string) {
	entityPropSet := make(map[string]property)
	for _, prop := range entityProps {
		entityPropSet[prop.Name] = prop
	}

	var orderedFields []string
	if queryStr != "" {
		for key := range firstEntry {
			orderedFields = append(orderedFields, key)
		}
		sort.SliceStable(orderedFields, func(i, j int) bool {
			return strings.Index(queryStr, orderedFields[i]) < strings.Index(queryStr, orderedFields[j])
		})
	} else {
		for _, prop := range entityProps {
			if _, ok := firstEntry[prop.Name]; ok {
				orderedFields = append(orderedFields, prop.Name)
			}
		}
	}

	for _, fieldName := range orderedFields {
		val := firstEntry[fieldName]
		typ := inferType(fieldName, val, entityProps)
		field := data.NewField(fieldName, nil, odata.ToArray(typ))
		frame.Fields = append(frame.Fields, field)
	}
}

func (ds *ODataSource) appendRow(frame *data.Frame, entry map[string]interface{}, entityProps []property) {
	values := make([]interface{}, len(frame.Fields))
	for i, field := range frame.Fields {
		rawValue, ok := entry[field.Name]
		if !ok {
			values[i] = nil
			continue
		}
		typ := inferType(field.Name, rawValue, entityProps)
		values[i] = odata.MapValue(rawValue, typ)
	}
	frame.AppendRow(values...)
}

func inferType(fieldName string, value interface{}, props []property) string {
	for _, prop := range props {
		if prop.Name == fieldName {
			return prop.Type
		}
	}

	switch v := value.(type) {
	case int, int8, int16, int32:
		return "Edm.Int32"
	case int64:
		return "Edm.Int64"
	case float32, float64:
		return "Edm.Decimal"
	case bool:
		return "Edm.Boolean"
	case string:
		if _, err := time.Parse(time.RFC3339, v); err == nil {
			return "Edm.DateTimeOffset"
		}
		return "Edm.String"
	default:
		return "Edm.String"
	}
}

func fieldExists(fields []*data.Field, name string) bool {
	for _, f := range fields {
		if f.Name == name {
			return true
		}
	}
	return false
}
