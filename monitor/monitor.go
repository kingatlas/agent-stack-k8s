package monitor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/buildkite/agent-stack-k8s/api"
	lru "github.com/hashicorp/golang-lru/v2"
	"go.uber.org/zap"
)

type Monitor struct {
	ctx         context.Context
	client      graphql.Client
	logger      *zap.Logger
	knownBuilds *lru.Cache[string, struct{}]
	cfg         Config
	jobs        chan Job
	once        sync.Once
}

type Config struct {
	Token       string
	MaxInFlight int
	Org         string
	Tags        []string
}

type Job struct {
	api.CommandJob
	Err error
}

func New(ctx context.Context, logger *zap.Logger, cfg Config) (*Monitor, error) {
	graphqlClient := api.NewClient(cfg.Token)
	length := cfg.MaxInFlight * 10
	if cfg.MaxInFlight == 0 {
		// there are other protections for
		// ensuring no duplicate jobs
		// this length just is an early-stage protection against duplicate
		// jobs in flight
		length = 1000
	}
	cache, err := lru.New[string, struct{}](length)
	if err != nil {
		return nil, err
	}
	return &Monitor{
		ctx:         ctx,
		client:      graphqlClient,
		logger:      logger,
		knownBuilds: cache,
		cfg:         cfg,
		jobs:        make(chan Job),
	}, nil
}

func (m *Monitor) Scheduled() <-chan Job {
	go m.once.Do(func() { go m.start() })
	return m.jobs
}

func (m *Monitor) Done(uuid string) {
	m.knownBuilds.Remove(uuid)
}

func (m *Monitor) start() {
	m.logger.Debug("started", zap.Strings("tags", m.cfg.Tags))
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			for _, tag := range m.cfg.Tags {
				buildsResponse, err := api.GetScheduledBuilds(m.ctx, m.client, m.cfg.Org, []string{tag})
				if err != nil {
					if errors.Is(err, context.Canceled) {
						continue
					}
					m.logger.Warn("failed to retrieve builds for pipeline", zap.Error(err))
					continue
				}
				if buildsResponse.Organization.Id == nil {
					m.jobs <- Job{Err: fmt.Errorf("invalid organization: %s", m.cfg.Org)}
				}
				builds := buildsResponse.Organization.Jobs.Edges
				sort.Slice(builds, func(i, j int) bool {
					cmdI := builds[i].Node.(*api.JobJobTypeCommand)
					cmdJ := builds[j].Node.(*api.JobJobTypeCommand)

					return cmdI.ScheduledAt.Before(cmdJ.ScheduledAt)

				})

				for _, job := range builds {
					cmdJob := job.Node.(*api.JobJobTypeCommand)
					if m.knownBuilds.Contains(cmdJob.Uuid) {
						m.logger.Debug("skipping already queued job", zap.String("uuid", cmdJob.Uuid))
					} else if inFlight := m.knownBuilds.Len(); m.cfg.MaxInFlight != 0 && inFlight >= m.cfg.MaxInFlight {
						m.logger.Debug("max in flight reached", zap.Int("in-flight", inFlight), zap.Int("max-in-flight", m.cfg.MaxInFlight))
					} else {
						m.logger.Debug("adding job", zap.String("uuid", cmdJob.Uuid))
						m.jobs <- Job{CommandJob: cmdJob.CommandJob}
						m.logger.Debug("added job", zap.String("uuid", cmdJob.Uuid))
						m.knownBuilds.Add(cmdJob.Uuid, struct{}{})
					}
				}
			}
		}
	}
}
