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

	// application/x-www-form-urlencoded causes the server's web framework to
	// URL-decode all values before passing them to the OData parser, which means
	// %26 in a value is decoded to a literal '&' (e.g. Material_Name eq 'Clone&1').
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if client.cookieHeader != "" {
		req.Header.Set("Cookie", client.cookieHeader)
	}

	return client.httpClient.Do(req)
}

func processURL(encodedURL string) (string, string) {
    urlParts := strings.SplitN(encodedURL, "?", 2)
    baseUrl := urlParts[0]
    queryString := ""
    if len(urlParts) > 1 {
        queryString = urlParts[1]
    }

    // URL-decode the raw query so that %26 becomes a literal &.
    decoded, err := url.QueryUnescape(queryString)
    if err != nil {
        decoded = queryString
    }

    // Split on the '&' characters that act as query-option separators (those
    // outside OData single-quoted string literals), then re-encode each option's
    // value with formEncodeODataValue. This produces an application/x-www-form-urlencoded
    // body where '&' inside a string value (e.g. Material_Name eq 'Clone&1') is
    // safely encoded as %26, while preserving OData syntax characters like commas.
    // The server's web framework decodes the form fields before passing them to
    // the OData parser, so the OData layer sees the correct literal '&' in the
    // filter expression and correct commas in the select list.
    options := splitOnOuterAmpersands(decoded)
    encoded := make([]string, 0, len(options))
    for _, opt := range options {
        eqIdx := strings.IndexByte(opt, '=')
        if eqIdx < 0 {
            encoded = append(encoded, opt)
            continue
        }
        key := opt[:eqIdx]
        value := opt[eqIdx+1:]
        encoded = append(encoded, key+"="+formEncodeODataValue(value))
    }

    postURL := fmt.Sprintf("%s?$query", baseUrl)
    return postURL, strings.Join(encoded, "&")
}

// formEncodeODataValue encodes a value for application/x-www-form-urlencoded
// with minimal encoding. Some OData servers don't properly decode form bodies,
// so we only encode the '&' character which would otherwise break the form
// field separation. All OData syntax (spaces, commas, quotes, etc.) is preserved.
func formEncodeODataValue(s string) string {
    // Only replace & with %26 to prevent it from being interpreted as a field separator.
    // Leave everything else (including spaces, commas, etc.) as-is because the OData
    // server expects the raw OData syntax in the body.
    return strings.ReplaceAll(s, "&", "%26")
}

// splitOnOuterAmpersands splits s on every '&' that appears outside an OData
// single-quoted string literal, returning the resulting segments. Escaped single
// quotes ('') inside a literal are handled correctly.
func splitOnOuterAmpersands(s string) []string {
    var result []string
    inString := false
    start := 0
    for i := 0; i < len(s); i++ {
        c := s[i]
        switch {
        case c == '\'':
            if inString && i+1 < len(s) && s[i+1] == '\'' {
                i++ // escaped quote
            } else {
                inString = !inString
            }
        case c == '&' && !inString:
            result = append(result, s[start:i])
            start = i + 1
        }
    }
    return append(result, s[start:])
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
