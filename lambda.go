// Lambda mode: one scheduled invocation fetches, renders, and uploads both
// cards to S3, where CloudFront serves them.
//
// Configuration comes from an S3 object read at cold start (the deploy
// bucket is the source of truth between deploys); only the token and the
// object locations arrive through the environment. A change to the config
// object takes effect when the execution environment next recycles.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/thesimpledev/gitdisplay/internal/config"
	"github.com/thesimpledev/gitdisplay/internal/gh"
	"github.com/thesimpledev/gitdisplay/internal/render"
	"github.com/thesimpledev/gitdisplay/internal/stats"
)

// lambdaEnv is everything Lambda mode takes from the environment.
type lambdaEnv struct {
	configBucket string
	configKey    string
	cardsBucket  string
	cardsPrefix  string
	token        string
}

// readLambdaEnv collects the required variables. Everything else about a run
// comes from the config object these point at.
func readLambdaEnv() (lambdaEnv, error) {
	var env lambdaEnv
	for _, v := range []struct {
		name string
		dst  *string
	}{
		{"CONFIG_BUCKET", &env.configBucket},
		{"CONFIG_KEY", &env.configKey},
		{"CARDS_BUCKET", &env.cardsBucket},
		{"CARDS_PREFIX", &env.cardsPrefix},
		{"GITHUB_TOKEN", &env.token},
	} {
		val := strings.TrimSpace(os.Getenv(v.name))
		if val == "" {
			return env, fmt.Errorf("%s is not set", v.name)
		}
		*v.dst = val
	}
	return env, nil
}

// runLambda wires up the handler and hands control to the Lambda runtime.
func runLambda() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	env, err := readLambdaEnv()
	if err != nil {
		return err
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}
	s3c := s3.NewFromConfig(awsCfg)

	cfg, err := fetchConfig(ctx, s3c, env.configBucket, env.configKey)
	if err != nil {
		return err
	}

	client := gh.New(cfg.GitHub.Endpoint, env.token, cfg.GitHub.UserAgent,
		cfg.GitHub.Timeout.D(), cfg.GitHub.MaxRetries)

	p := &publisher{cfg: cfg, client: client, s3: s3c,
		bucket: env.cardsBucket, prefix: env.cardsPrefix}
	lambda.Start(p.handle)
	return nil
}

// fetchConfig reads and validates the config JSON from S3.
func fetchConfig(ctx context.Context, s3c *s3.Client, bucket, key string) (config.Config, error) {
	out, err := s3c.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return config.Default(), fmt.Errorf("get config s3://%s/%s: %w", bucket, key, err)
	}
	defer func() {
		_ = out.Body.Close() // read side already decided success or failure
	}()

	raw, err := io.ReadAll(io.LimitReader(out.Body, 1<<20))
	if err != nil {
		return config.Default(), fmt.Errorf("read config s3://%s/%s: %w", bucket, key, err)
	}
	cfg, err := config.LoadBytes(raw)
	if err != nil {
		return cfg, fmt.Errorf("config s3://%s/%s: %w", bucket, key, err)
	}
	return cfg, nil
}

// publisher holds what one invocation needs to fetch, render, and upload.
type publisher struct {
	cfg    config.Config
	client *gh.Client
	s3     *s3.Client
	bucket string
	prefix string
}

// card is one rendered SVG ready to upload.
type card struct {
	name   string
	svg    []byte
	maxAge time.Duration
}

// handle is one scheduled run: fetch, render every enabled card, and only
// then upload. Uploads are sequential and not atomic; if the second fails
// the first stays published, which at worst leaves the pair briefly
// mismatched rather than broken.
func (p *publisher) handle(ctx context.Context) error {
	// "Today" must be the invocation's day, not the cold start's: a warm
	// execution environment can live for days and the streak depends on it.
	now := time.Now().In(p.cfg.Location())
	a := &app{cfg: p.cfg, client: p.client, now: now}

	data, err := p.client.Fetch(ctx,
		a.fetchOptions(p.cfg.Languages.Enabled, p.cfg.Streak.Enabled))
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}

	cards, err := a.renderCards(data)
	if err != nil {
		return err
	}

	for i, c := range cards {
		if putErr := p.put(ctx, c); putErr != nil {
			if i > 0 {
				log.Printf("PARTIAL UPLOAD: %d of %d cards uploaded before this failure; the pair may be mismatched until the next run", i, len(cards))
			}
			return putErr
		}
		log.Printf("uploaded %s (%d bytes)", c.name, len(c.svg))
	}
	return nil
}

// renderCards renders every enabled card. It refuses to hand back a card
// that would publish empty (no languages after filtering, no calendars),
// keeping the previous render in place exactly as the old refresh loops did.
func (a *app) renderCards(data *gh.Data) ([]card, error) {
	if data == nil {
		return nil, errors.New("render: nil data")
	}
	var out []card
	if a.cfg.Languages.Enabled {
		svg, res := a.renderLangs(data)
		if len(res.Languages) == 0 {
			return nil, errors.New("no languages after filtering; keeping the previous card")
		}
		reportLangs(res)
		out = append(out, card{name: CardLangs, svg: svg, maxAge: a.cfg.LangsMaxAge.D()})
	}
	if a.cfg.Streak.Enabled {
		if len(data.Calendars) == 0 {
			return nil, errors.New("no contribution calendars returned; keeping the previous card")
		}
		res := stats.Streaks(data, a.cfg.Streak, a.cfg.Location(), a.now)
		reportStreak(res)
		out = append(out, card{name: CardStreak, svg: render.Streak(res, a.cfg.Streak),
			maxAge: a.cfg.StreakMaxAge.D()})
	}
	return out, nil
}

// put uploads one card with the headers CloudFront passes through.
func (p *publisher) put(ctx context.Context, c card) error {
	if len(c.svg) == 0 {
		return fmt.Errorf("refusing to upload empty %s", c.name)
	}
	key := strings.TrimSuffix(p.prefix, "/") + "/" + c.name + ".svg"
	_, err := p.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(p.bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(c.svg),
		ContentType:  aws.String("image/svg+xml; charset=utf-8"),
		CacheControl: aws.String(fmt.Sprintf("public, max-age=%d", int(c.maxAge.Seconds()))),
	})
	if err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", p.bucket, key, err)
	}
	return nil
}
