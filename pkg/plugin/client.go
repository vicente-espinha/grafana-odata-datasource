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
		parsedBaseUrl, err := url.Parse(client.baseUrl)
		if err != nil {
			return nil, err
		}

		parsedQuery, err := url.Parse(oDataQueryString)
		if err != nil {
			return nil, err
		}

		parsedBaseUrl.Path = path.Join(parsedBaseUrl.Path, parsedQuery.Path)

		// Preserve RawQuery directly instead of going through url.ParseQuery + Encode,
		// because ParseQuery splits on '&' which can legitimately appear inside OData
		// string literals (e.g. Material_Name in ('Clone&1',...)).
		if parsedBaseUrl.RawQuery != "" && parsedQuery.RawQuery != "" {
			parsedBaseUrl.RawQuery = parsedBaseUrl.RawQuery + "&" + parsedQuery.RawQuery
		} else {
			parsedBaseUrl.RawQuery = parsedQuery.RawQuery
		}

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
    baseUrl := parts[0]
    queryString := ""
    if len(parts) > 1 {
        queryString = parts[1]
    }

    // Encode any literal '&' inside OData single-quoted string literals to %26 so
    // the OData server doesn't misinterpret them as query-option separators.
    // This handles both a frontend that sends 'Clone&1' (literal) and one that
    // already sends 'Clone%261' (pre-encoded) — %26 is not touched here.
    url := fmt.Sprintf("%s?$query", baseUrl)
    return url, encodeAmpersandInStringLiterals(queryString)
}

// encodeAmpersandInStringLiterals replaces literal '&' characters that appear
// inside OData single-quoted string literals with their percent-encoded form %26.
// Escaped single quotes ('') inside a literal are handled correctly.
func encodeAmpersandInStringLiterals(s string) string {
    var sb strings.Builder
    inString := false
    for i := 0; i < len(s); i++ {
        c := s[i]
        switch {
        case c == '\'':
            // OData escapes a single quote inside a string as ''
            if inString && i+1 < len(s) && s[i+1] == '\'' {
                sb.WriteByte(c)
                sb.WriteByte(s[i+1])
                i++
            } else {
                inString = !inString
                sb.WriteByte(c)
            }
        case c == '&' && inString:
            sb.WriteString("%26")
        default:
            sb.WriteByte(c)
        }
    }
    return sb.String()
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
