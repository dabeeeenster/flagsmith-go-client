package flagsmith

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/Flagsmith/flagsmith-go-client/v2/flagengine"
	"github.com/Flagsmith/flagsmith-go-client/v2/flagengine/environments"
	"github.com/Flagsmith/flagsmith-go-client/v2/flagengine/identities"
	"github.com/Flagsmith/flagsmith-go-client/v2/flagengine/segments"

	. "github.com/Flagsmith/flagsmith-go-client/v2/flagengine/identities/traits"
)

// Client provides various methods to query Flagsmith API
type Client struct {
	apiKey string
	config config

	environment atomic.Value

	analyticsProcessor *AnalyticsProcessor
	defaultFlagHandler func(string) Flag

	client *resty.Client
	ctx    context.Context
	log    Logger
}

// NewClient creates instance of Client with given configuration
func NewClient(apiKey string, options ...Option) *Client {
	c := &Client{
		apiKey: apiKey,
		config: defaultConfig(),
		client: resty.New(),
		ctx:    context.Background(),
	}

	c.client.SetHeaders(map[string]string{
		"Accept":            "application/json",
		"X-Environment-Key": c.apiKey,
	})
	c.client.SetTimeout(c.config.timeout)
	c.log = createLogger()

	for _, opt := range options {
		opt(c)

	}
	if c.config.localEvaluation {
		go c.pollEnvironment(c.ctx)
	}
	// Initialize analytics processor
	if c.config.enableAnalytics {
		c.analyticsProcessor = NewAnalyticsProcessor(c.ctx, c.client, c.config.baseURL, nil, c.log)
	}

	return c
}

// Returns `Flags` struct holding all the flags for the current environment
func (c *Client) GetEnvironmentFlags() (Flags, error) {
	if env, ok := c.environment.Load().(*environments.EnvironmentModel); ok {
		return c.GetEnvironmentFlagsFromDocument(c.ctx, env)

	}
	return c.GetEnvironmentFlagsFromAPI(c.ctx)
}

// Returns `Flags` struct holding all the flags for the current environment for a given identity. Will also
// upsert all traits to the Flagsmith API for future evaluations. Providing a
// trait with a value of nil will remove the trait from the identity if it exists.
func (c *Client) GetIdentityFlags(identifier string, traits []*Trait) (Flags, error) {
	if env, ok := c.environment.Load().(*environments.EnvironmentModel); ok {
		return c.GetIdentityFlagsFromDocument(c.ctx, env, identifier, traits)

	}
	return c.GetIdentityFlagsFromAPI(c.ctx, identifier, traits)

}

// Returns an array of segments that the given identity is part of
func (c *Client) GeIdentitySegments(identifier string, traits []*Trait) ([]*segments.SegmentModel, error) {
	if env, ok := c.environment.Load().(*environments.EnvironmentModel); ok {
		identity := buildIdentityModel(identifier, env.APIKey, traits)
		return flagengine.GetIdentitySegments(env, &identity), nil
	}
	return nil, &FlagsmithClientError{msg: "flagsmith: Local evaluation required to obtain identity segments"}

}

// BulkIdentify can be used to create/overwrite identities(with traits) in bulk
// NOTE: This method only works with Edge API endpoint
func (c *Client) BulkIdentify(batch []*IdentityTraits) error {
	if len(batch) > bulkIdentifyMaxCount {
		return &FlagsmithAPIError{msg: fmt.Sprintf("flagsmith: batch size must be less than %d", bulkIdentifyMaxCount)}
	}

	body := struct {
		Data []*IdentityTraits `json:"data"`
	}{Data: batch}

	resp, err := c.client.NewRequest().
		SetBody(&body).
		SetContext(c.ctx).
		Post(c.config.baseURL + "bulk-identities/")
	if resp.StatusCode() == 404 {
		return &FlagsmithAPIError{msg: "flagsmith: Bulk identify endpoint not found; Please make sure you are using Edge API endpoint"}
	}
	if err != nil || !resp.IsSuccess() {
		return &FlagsmithAPIError{msg: "flagsmith: Unable to get valid response from Flagsmith API"}
	}
	return nil
}

func (c *Client) GetEnvironmentFlagsFromAPI(ctx context.Context) (Flags, error) {
	resp, err := c.client.NewRequest().
		SetContext(ctx).
		Get(c.config.baseURL + "flags/")

	if err != nil || !resp.IsSuccess() {
		if c.defaultFlagHandler != nil {
			return Flags{defaultFlagHandler: c.defaultFlagHandler}, nil
		}
		return Flags{}, &FlagsmithAPIError{msg: "flagsmith: Unable to get valid response from Flagsmith API"}
	}
	return makeFlagsFromAPIFlags(resp.Body(), c.analyticsProcessor, c.defaultFlagHandler)

}

func (c *Client) GetIdentityFlagsFromAPI(ctx context.Context, identifier string, traits []*Trait) (Flags, error) {
	body := struct {
		Identifier string   `json:"identifier"`
		Traits     []*Trait `json:"traits,omitempty"`
	}{Identifier: identifier, Traits: traits}
	resp, err := c.client.NewRequest().
		SetBody(&body).
		SetContext(ctx).
		Post(c.config.baseURL + "identities/")
	if err != nil || !resp.IsSuccess() {
		if c.defaultFlagHandler != nil {
			return Flags{defaultFlagHandler: c.defaultFlagHandler}, nil
		}
		return Flags{}, &FlagsmithAPIError{msg: "flagsmith: Unable to get valid response from Flagsmith API"}
	}
	return makeFlagsfromIdentityAPIJson(resp.Body(), c.analyticsProcessor, c.defaultFlagHandler)

}

func (c *Client) GetIdentityFlagsFromDocument(ctx context.Context, env *environments.EnvironmentModel, identifier string, traits []*Trait) (Flags, error) {
	identity := buildIdentityModel(identifier, env.APIKey, traits)
	featureStates := flagengine.GetIdentityFeatureStates(env, &identity)
	flags := makeFlagsFromFeatureStates(
		featureStates,
		c.analyticsProcessor,
		c.defaultFlagHandler,
		identifier,
	)
	return flags, nil
}

func (c *Client) GetEnvironmentFlagsFromDocument(ctx context.Context, env *environments.EnvironmentModel) (Flags, error) {
	return makeFlagsFromFeatureStates(
		env.FeatureStates,
		c.analyticsProcessor,
		c.defaultFlagHandler,
		"",
	), nil
}

func (c *Client) pollEnvironment(ctx context.Context) {
	update := func() {
		ctx, cancel := context.WithTimeout(ctx, c.config.envRefreshInterval)
		defer cancel()
		err := c.UpdateEnvironment(ctx)
		if err != nil {
			c.log.Errorf("Failed to update environment: %v", err)
		}
	}
	update()
	ticker := time.NewTicker(c.config.envRefreshInterval)
	for {
		select {
		case <-ticker.C:
			update()
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) UpdateEnvironment(ctx context.Context) error {
	var env environments.EnvironmentModel
	e := make(map[string]string)
	_, err := c.client.NewRequest().
		SetContext(ctx).
		SetResult(&env).
		SetError(&e).
		Get(c.config.baseURL + "environment-document/")

	if err != nil {
		return err
	}
	if len(e) > 0 {
		return errors.New(e["detail"])
	}
	c.environment.Store(&env)

	return nil
}

func buildIdentityModel(identifier string, apiKey string, traits []*Trait) identities.IdentityModel {
	identityTraits := make([]*TraitModel, len(traits))
	for i, trait := range traits {
		identityTraits[i] = trait.ToTraitModel()
	}
	return identities.IdentityModel{
		Identifier:        identifier,
		IdentityTraits:    identityTraits,
		EnvironmentAPIKey: apiKey,
	}
}
