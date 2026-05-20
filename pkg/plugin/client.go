package plugin

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/d-velop/grafana-odata-datasource/pkg/plugin/odata"
	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

type ODataClient interface {
	GetServiceRoot() (*http.Response, error)
	GetMetadata() (*http.Response, error)
	Get(oDataQueryString string, entitySet string, properties []property, filterConditions []filterCondition, usePost bool) (*http.Response, error)
}

type ODataClientImpl struct {
	httpClient       *http.Client
	baseUrl          string
	urlSpaceEncoding string
	cookieHeader     string
}

func (c *ODataClientImpl) SetCookieHeader(header string) {
	c.cookieHeader = header
}

func (client *ODataClientImpl) GetServiceRoot() (*http.Response, error) {
	return client.doGetRequest(client.baseUrl)
}

func (client *ODataClientImpl) GetMetadata() (*http.Response, error) {
	requestUrl, err := url.Parse(client.baseUrl)
	if err != nil {
		return nil, err
	}
	requestUrl.Path = path.Join(requestUrl.Path, odata.Metadata)
	return client.doGetRequest(requestUrl.String())
}

func (client *ODataClientImpl) Get(oDataQueryString string, entitySet string, properties []property, filterConditions []filterCondition, usePost bool) (*http.Response, error) {
	var requestUrl string

	if oDataQueryString != "" {
		// Encode literal & characters inside OData single-quoted string literals
		// as %26 before URL parsing. Grafana template variable substitution can
		// inject values containing & (e.g. a material name like 'Clone&1') as
		// plain text, making the & indistinguishable from a query-string
		// parameter separator. This must happen before url.Parse so the raw
		// query is correct from the start.
		oDataQueryString = encodeAmpersandInLiterals(oDataQueryString)

		parsedBaseUrl, err := url.Parse(client.baseUrl)
		if err != nil {
			return nil, err
		}

		parsedQuery, err := url.Parse(oDataQueryString)
		if err != nil {
			return nil, err
		}

		parsedBaseUrl.Path = path.Join(parsedBaseUrl.Path, parsedQuery.Path)

		// Merge raw query strings without the decode/re-encode cycle that
		// url.ParseQuery + url.Values.Encode() would apply. That cycle breaks
		// two things when & appears in an OData value (e.g. a material name):
		//   1. url.ParseQuery splits on a literal & inside a value, truncating $filter.
		//   2. url.Values.Encode re-encodes $ in OData parameter names to %24
		//      (e.g. $filter → %24filter), which many OData servers reject.
		// By merging the raw query strings we preserve the caller's encoding,
		// including %26 for & in values and $ in OData parameter names.
		rawQuery := parsedQuery.RawQuery
		if parsedBaseUrl.RawQuery != "" {
			rawQuery = parsedBaseUrl.RawQuery + "&" + rawQuery
		}
		// Encode any literal spaces present in the OData expression.
		spaceEncoding := "+"
		if client.urlSpaceEncoding == "%20" {
			spaceEncoding = "%20"
		}
		rawQuery = strings.ReplaceAll(rawQuery, " ", spaceEncoding)
		parsedBaseUrl.RawQuery = rawQuery

		requestUrl = parsedBaseUrl.String()
		log.DefaultLogger.Debug("Using provided OData query string: " + requestUrl)
	} else {
		builtUrl, err := buildQueryUrl(client.baseUrl, entitySet, properties, filterConditions, client.urlSpaceEncoding)
		if err != nil {
			return nil, err
		}
		requestUrl = builtUrl.String()
		log.DefaultLogger.Debug("Constructed request url: ", requestUrl)
	}

	if usePost {
		return client.doPostRequest(requestUrl)
	}

	return client.doGetRequest(requestUrl)
}

func (client *ODataClientImpl) doGetRequest(urlToGet string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, urlToGet, nil)
	if err != nil {
		return nil, err
	}
	if client.cookieHeader != "" {
		req.Header.Set("Cookie", client.cookieHeader)
	}
	return client.httpClient.Do(req)
}

func (client *ODataClientImpl) doPostRequest(urlToPost string) (*http.Response, error) {
	newURL, body := processURL(urlToPost)

	req, err := http.NewRequest(http.MethodPost, newURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "text/plain")
	if client.cookieHeader != "" {
		req.Header.Set("Cookie", client.cookieHeader)
	}

	return client.httpClient.Do(req)
}

func processURL(encodedURL string) (string, string) {
    parts := strings.SplitN(encodedURL, "?", 2)

    baseURL := parts[0]
    queryString := ""

    if len(parts) > 1 {
        queryString = parts[1]
    }

    queryString = strings.ReplaceAll(queryString, "+", "%20")

    queryURL := fmt.Sprintf("%s?$query", baseURL)

    return queryURL, queryString
}

func buildQueryUrl(baseUrl string, entitySet string, properties []property, filterConditions []filterCondition, urlSpaceEncoding string) (*url.URL, error) {
	requestUrl, err := url.Parse(baseUrl)
	if err != nil {
		return nil, err
	}
	requestUrl.Path = path.Join(requestUrl.Path, entitySet)
	params, _ := url.ParseQuery(requestUrl.RawQuery)
	filterParam := mapFilter(filterConditions)
	if len(filterParam) > 0 {
		params.Add(odata.Filter, filterParam)
	}
	selectParam := mapSelect(properties)
	if len(selectParam) > 0 {
		params.Add(odata.Select, selectParam)
	}
	encodedUrl := params.Encode()
	if urlSpaceEncoding == "%20" {
		encodedUrl = strings.ReplaceAll(encodedUrl, "+", "%20")
	}
	requestUrl.RawQuery = encodedUrl
	return requestUrl, nil
}

// encodeAmpersandInLiterals replaces literal & characters that appear inside
// OData single-quoted string literals with %26. This prevents them from being
// misinterpreted as query-string parameter separators when the & was injected
// by Grafana template variable substitution (e.g. a variable value 'Clone&1'
// becomes %26-encoded so it stays within the surrounding in(...) clause).
// Characters outside single quotes are left untouched, so the & separators
// between OData system query options ($filter, $select, $orderby, ...) are
// preserved. Escaped single quotes in OData (written as '') are handled
// correctly and do not prematurely end the string scan.
func encodeAmpersandInLiterals(s string) string {
	var b strings.Builder
	inLiteral := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			// An escaped single quote in OData is two consecutive apostrophes.
			// Consume both without toggling the literal flag.
			if inLiteral && i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				b.WriteByte('\'')
				i++
				continue
			}
			inLiteral = !inLiteral
			b.WriteByte(c)
		case c == '&' && inLiteral:
			b.WriteString("%26")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func mapSelect(properties []property) string {
	var result []string
	if len(properties) > 0 {
		for _, selectProp := range properties {
			result = append(result, selectProp.Name)
		}
	}
	return strings.Join(result[:], ",")
}

func mapFilter(filterConditions []filterCondition) string {
	var filter = ""
	for index, element := range filterConditions {
		if element.Property.Type == odata.EdmString {
			filter += fmt.Sprintf("%s %s '%s'", element.Property.Name, element.Operator, element.Value)
		} else {
			filter += fmt.Sprintf("%s %s %s", element.Property.Name, element.Operator, element.Value)
		}
		if index < (len(filterConditions) - 1) {
			filter += " and "
		}
	}

	return filter
}
