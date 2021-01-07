package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"

	"github.com/InoiOy/go-graphql-client/internal/jsonutil"
	"golang.org/x/net/context/ctxhttp"
)

// Client is a GraphQL client.
type Client struct {
	url        string // GraphQL server URL.
	httpClient *http.Client
}

// NewClient creates a GraphQL client targeting the specified GraphQL server URL.
// If httpClient is nil, then http.DefaultClient is used.
func NewClient(url string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		url:        url,
		httpClient: httpClient,
	}
}

// Query executes a single GraphQL query request,
// with a query derived from q, populating the response into it.
// q should be a pointer to struct that corresponds to the GraphQL schema.
func (c *Client) Query(ctx context.Context, q interface{}, variables map[string]interface{}) error {
	return c.do(ctx, queryOperation, q, variables, "")
}

// NamedQuery executes a single GraphQL query request, with operation name
func (c *Client) NamedQuery(ctx context.Context, name string, q interface{}, variables map[string]interface{}) error {
	return c.do(ctx, queryOperation, q, variables, name)
}

// Mutate executes a single GraphQL mutation request,
// with a mutation derived from m, populating the response into it.
// m should be a pointer to struct that corresponds to the GraphQL schema.
func (c *Client) Mutate(ctx context.Context, m interface{}, variables map[string]interface{}) error {
	return c.do(ctx, mutationOperation, m, variables, "")
}

// NamedMutate executes a single GraphQL mutation request, with operation name
func (c *Client) NamedMutate(ctx context.Context, name string, m interface{}, variables map[string]interface{}) error {
	return c.do(ctx, mutationOperation, m, variables, name)
}

// Query executes a single GraphQL query request,
// with a query derived from q, populating the response into it.
// q should be a pointer to struct that corresponds to the GraphQL schema.
// return raw bytes message.
func (c *Client) QueryRaw(ctx context.Context, q interface{}, variables map[string]interface{}) (*json.RawMessage, error) {
	return c.doRaw(ctx, queryOperation, q, variables, "")
}

// NamedQueryRaw executes a single GraphQL query request, with operation name
// return raw bytes message.
func (c *Client) NamedQueryRaw(ctx context.Context, name string, q interface{}, variables map[string]interface{}) (*json.RawMessage, error) {
	return c.doRaw(ctx, queryOperation, q, variables, name)
}

// MutateRaw executes a single GraphQL mutation request,
// with a mutation derived from m, populating the response into it.
// m should be a pointer to struct that corresponds to the GraphQL schema.
// return raw bytes message.
func (c *Client) MutateRaw(ctx context.Context, m interface{}, variables map[string]interface{}) (*json.RawMessage, error) {
	return c.doRaw(ctx, mutationOperation, m, variables, "")
}

// NamedMutateRaw executes a single GraphQL mutation request, with operation name
// return raw bytes message.
func (c *Client) NamedMutateRaw(ctx context.Context, name string, m interface{}, variables map[string]interface{}) (*json.RawMessage, error) {
	return c.doRaw(ctx, mutationOperation, m, variables, name)
}

// do executes a single GraphQL operation.
// return raw message and error
func (c *Client) doRaw(ctx context.Context, op operationType, v interface{}, variables map[string]interface{}, name string) (*json.RawMessage, error) {
	var query string
	switch op {
	case queryOperation:
		query = constructQuery(v, variables, name)
	case mutationOperation:
		query = constructMutation(v, variables, name)
	}
	var response responseBody
	if err := c.createRequest(ctx, query, variables, &response); err != nil {
		return nil, err
	}
	if len(response.Errors) > 0 {
		return response.Data, response.Errors
	}

	return response.Data, nil
}

// do executes a single GraphQL operation and unmarshal json.
func (c *Client) do(ctx context.Context, op operationType, v interface{}, variables map[string]interface{}, name string) error {
	var query string
	switch op {
	case queryOperation:
		query = constructQuery(v, variables, name)
	case mutationOperation:
		query = constructMutation(v, variables, name)
	}

	var response responseBody
	if err := c.createRequest(ctx, query, variables, &response); err != nil {
		return err
	}
	if response.Data != nil {
		err := jsonutil.UnmarshalGraphQL(*response.Data, v)
		if err != nil {
			return err
		}
	}
	if len(response.Errors) > 0 {
		return response.Errors
	}

	return nil
}

func (c *Client) createRequest(ctx context.Context, query string, variables map[string]interface{}, response interface{}) error {
	in := struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables,omitempty"`
	}{
		Query:     query,
		Variables: variables,
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(in); err != nil {
		return err
	}
	resp, err := ctxhttp.Post(ctx, c.httpClient, c.url, "application/json", &buf)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("non-200 OK status code: %v body: %q", resp.Status, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return err
	}

	return nil
}

// errors represents the "errors" array in a response from a GraphQL server.
// If returned via error interface, the slice is expected to contain at least 1 element.
//
// Specification: https://facebook.github.io/graphql/#sec-Errors.
type errors []struct {
	Message   string
	Locations []struct {
		Line   int
		Column int
	}
}

type responseBody struct {
	Data   *json.RawMessage
	Errors errors
}

// Error implements error interface.
func (e errors) Error() string {
	return e[0].Message
}

type operationType uint8

const (
	queryOperation operationType = iota
	mutationOperation
	//subscriptionOperation // Unused.
)
