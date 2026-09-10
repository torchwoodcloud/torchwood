package idgen

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	domainidgen "github.com/torchwoodcloud/torchwood/internal/domain/idgen"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	pkgidgen "github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// strategyCacheTTL 是项目 ID 生成策略查询结果的缓存时长：生成路径高频调用
// 不应每次打穿数据库。
const strategyCacheTTL = 30 * time.Second

// Service implements domainidgen.Generator using platform config and optional project overrides.
type Service struct {
	cfg               *config.AppConfig
	rdb               *redis.Client
	projectRepo       projects.Repository
	snowflake         *pkgidgen.Snowflake
	randomCfg         pkgidgen.RandomConfig
	randomPrefix      string
	seqPrefix         string
	defaultStrategy   string
	resourceUsers     string
	resourceSessions  string
	resourceDocuments string

	mu            sync.Mutex
	strategyCache map[string]strategyEntry
}

// strategyEntry 是单项目策略缓存的快照。
type strategyEntry struct {
	settings map[string]any
	expireAt time.Time
}

func NewService(cfg *config.AppConfig, rdb *redis.Client, projectRepo projects.Repository) (*Service, error) {
	nodeID := int64(0)
	randomLen := int32(10)
	randomCharset := pkgidgen.RandomCharsetNumeric
	randomPrefix := "Torchwood:id:random"
	randomMaxRetries := int32(10)
	seqPrefix := "Torchwood:seq"
	defaultStrategy := pkgidgen.StrategyUUID
	resourceUsers := ""
	resourceSessions := ""
	resourceDocuments := ""

	if cfg != nil && cfg.GetIdgen() != nil {
		idCfg := cfg.GetIdgen()
		defaultStrategy = pkgidgen.NormalizeStrategy(idCfg.GetDefaultStrategy())
		if idCfg.GetSnowflake() != nil {
			nodeID = int64(idCfg.GetSnowflake().GetNodeId())
		}
		if idCfg.GetRandom() != nil {
			randomLen = idCfg.GetRandom().GetLength()
			if c := strings.TrimSpace(idCfg.GetRandom().GetCharset()); c != "" {
				randomCharset = c
			}
			if p := strings.TrimSpace(idCfg.GetRandom().GetRedisKeyPrefix()); p != "" {
				randomPrefix = p
			}
			if idCfg.GetRandom().GetMaxRetries() > 0 {
				randomMaxRetries = idCfg.GetRandom().GetMaxRetries()
			}
		}
		if idCfg.GetSequence() != nil && strings.TrimSpace(idCfg.GetSequence().GetRedisKeyPrefix()) != "" {
			seqPrefix = strings.TrimSpace(idCfg.GetSequence().GetRedisKeyPrefix())
		}
		if idCfg.GetResources() != nil {
			resourceUsers = strings.TrimSpace(idCfg.GetResources().GetUsers())
			resourceSessions = strings.TrimSpace(idCfg.GetResources().GetSessions())
			resourceDocuments = strings.TrimSpace(idCfg.GetResources().GetDocuments())
		}
	}

	sf, err := pkgidgen.NewSnowflake(nodeID)
	if err != nil {
		return nil, err
	}

	return &Service{
		cfg:         cfg,
		rdb:         rdb,
		projectRepo: projectRepo,
		snowflake:   sf,
		randomCfg: pkgidgen.RandomConfig{
			Length:     int(randomLen),
			Charset:    randomCharset,
			MaxRetries: int(randomMaxRetries),
		}.WithDefaults(),
		randomPrefix:      randomPrefix,
		seqPrefix:         seqPrefix,
		defaultStrategy:   defaultStrategy,
		resourceUsers:     resourceUsers,
		resourceSessions:  resourceSessions,
		resourceDocuments: resourceDocuments,
	}, nil
}

var _ domainidgen.Generator = (*Service)(nil)

func (s *Service) NewID(ctx context.Context, projectID string, resource domainidgen.Resource) (string, error) {
	strategy, err := s.resolveStrategy(ctx, projectID, resource)
	if err != nil {
		return "", err
	}
	switch strategy {
	case pkgidgen.StrategyULID:
		return pkgidgen.ULID().String(), nil
	case pkgidgen.StrategySnowflake:
		return s.snowflake.NextString(), nil
	case pkgidgen.StrategySequence:
		return s.nextSequence(ctx, projectID, resource)
	case pkgidgen.StrategyRandom:
		return s.nextRandom(ctx, projectID, resource)
	default:
		return pkgidgen.UUID().String(), nil
	}
}

func (s *Service) resolveStrategy(ctx context.Context, projectID string, resource domainidgen.Resource) (string, error) {
	platformDefault := s.defaultStrategy
	switch resource {
	case domainidgen.ResourceUsers:
		if s.resourceUsers != "" {
			platformDefault = pkgidgen.NormalizeStrategy(s.resourceUsers)
		}
	case domainidgen.ResourceSessions:
		if s.resourceSessions != "" {
			platformDefault = pkgidgen.NormalizeStrategy(s.resourceSessions)
		}
	case domainidgen.ResourceDocuments:
		if s.resourceDocuments != "" {
			platformDefault = pkgidgen.NormalizeStrategy(s.resourceDocuments)
		}
	}

	if projectID == "" || s.projectRepo == nil {
		return platformDefault, nil
	}
	settings, err := s.projectSettings(ctx, projectID)
	if err != nil {
		// 不静默回退平台默认：无法确认项目覆盖时直接用错策略会破坏
		// 全局唯一性/顺序语义，宁可显式失败。
		return "", fmt.Errorf("resolve idgen strategy for project %q: %w", projectID, err)
	}
	if settings == nil {
		return platformDefault, nil
	}
	return pkgidgen.NormalizeStrategy(projects.IDGenStrategyForResource(settings, string(resource), platformDefault)), nil
}

// projectSettings 返回项目 settings（带短 TTL 缓存）；项目不存在返回
// (nil, nil)，DB 错误原样上抛。
func (s *Service) projectSettings(ctx context.Context, projectID string) (map[string]any, error) {
	now := time.Now()
	s.mu.Lock()
	if e, ok := s.strategyCache[projectID]; ok && now.Before(e.expireAt) {
		s.mu.Unlock()
		return e.settings, nil
	}
	s.mu.Unlock()

	project, err := s.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	if project != nil {
		settings = project.Settings
	}

	s.mu.Lock()
	if s.strategyCache == nil {
		s.strategyCache = make(map[string]strategyEntry)
	}
	s.strategyCache[projectID] = strategyEntry{settings: settings, expireAt: now.Add(strategyCacheTTL)}
	s.mu.Unlock()
	return settings, nil
}

func (s *Service) nextSequence(ctx context.Context, projectID string, resource domainidgen.Resource) (string, error) {
	if s.rdb == nil {
		return pkgidgen.UUID().String(), nil
	}
	key := fmt.Sprintf("%s:%s:%s", s.seqPrefix, projectID, resource)
	n, err := s.rdb.Incr(ctx, key).Result()
	if err != nil {
		return "", fmt.Errorf("sequence id generation failed: %w", err)
	}
	return fmt.Sprintf("%d", n), nil
}
