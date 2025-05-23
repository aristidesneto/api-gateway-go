package route

import (
	"context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"time"

	"github.com/diillson/api-gateway-go/internal/domain/model"
	"github.com/diillson/api-gateway-go/internal/domain/repository"
	"github.com/diillson/api-gateway-go/pkg/cache"
	"go.uber.org/zap"
)

type Service struct {
	repo   repository.RouteRepository
	cache  cache.Cache
	logger *zap.Logger
}

func NewService(repo repository.RouteRepository, cache cache.Cache, logger *zap.Logger) *Service {
	return &Service{
		repo:   repo,
		cache:  cache,
		logger: logger,
	}
}

// GetRoutes retorna todas as rotas ativas
func (s *Service) GetRoutes(ctx context.Context) ([]*model.Route, error) {
	var routes []*model.Route

	// Tentar cache primeiro
	cacheKey := "routes"
	found, err := s.cache.Get(ctx, cacheKey, &routes)
	if err != nil {
		s.logger.Error("Erro ao buscar rotas do cache", zap.Error(err))
		return nil, err
	}

	if found {
		return routes, nil
	}

	// Se não estiver no cache, buscar do repositório
	routes, err = s.repo.GetRoutes(ctx)
	if err != nil {
		return nil, err
	}

	// Armazenar no cache para futuras requisições
	if err := s.cache.Set(ctx, cacheKey, routes, 5*time.Minute); err != nil {
		s.logger.Warn("Erro ao armazenar rotas no cache", zap.Error(err))
	}

	return routes, nil
}

func (s *Service) GetRouteByPath(ctx context.Context, path string) (*model.Route, error) {
	// Obter o tracer atual do contexto
	tracer := otel.GetTracerProvider().Tracer("api-gateway.route.service")

	// Criar um span para esta operação
	ctx, span := tracer.Start(
		ctx,
		"RouteService.GetRouteByPath",
		trace.WithAttributes(
			attribute.String("route.path", path),
			attribute.String("operation", "route_lookup"),
		),
	)
	defer span.End()

	// Adicionar log para debug
	s.logger.Info("Buscando rota", zap.String("path", path))

	// Primeiro tentar cache individual da rota
	var route *model.Route
	routeCacheKey := "route:" + path

	found, err := s.cache.Get(ctx, routeCacheKey, &route)
	if err != nil {
		s.logger.Error("Erro ao verificar cache individual de rota",
			zap.String("path", path),
			zap.Error(err))
		// Continuamos a execução mesmo com erro no cache
	} else if found {
		// rota encontrada no cahe, adiciona log e trace
		s.logger.Info("Rota encontrada no cache individual",
			zap.String("path", path),
			zap.String("serviceURL", route.ServiceURL),
			zap.String("cache_key", routeCacheKey))

		// Add attributes to the span
		span.SetAttributes(
			attribute.String("route.service_url", route.ServiceURL),
			attribute.Bool("route.is_active", route.IsActive),
			attribute.Bool("route.from_cache", true),
			attribute.Bool("cache.hit", true),
		)

		span.SetStatus(codes.Ok, "rota encontrada no individual!")
		return route, nil
	}

	// Se não estiver no cache individual, buscar da lista de rotas (que pode estar em cache)
	var routes []*model.Route

	// Tentar cache para a lista de rotas
	cacheKey := "routes"
	found, err = s.cache.Get(ctx, cacheKey, &routes)
	if err != nil {
		s.logger.Error("Erro ao buscar rotas do cache", zap.Error(err))
		// Continuamos para buscar do repositório em caso de erro
	} else if found {
		s.logger.Debug("Lista de rotas encontrada no cache",
			zap.Int("routes_count", len(routes)))
		span.SetAttributes(attribute.Bool("routes_list.from_cache", true))
	} else {
		// Se não estiver no cache, buscar do repositório
		s.logger.Info("Lista de rotas não encontrada no cache, buscando do repositório")
		routes, err = s.repo.GetRoutes(ctx)
		if err != nil {
			s.logger.Error("Erro ao buscar rotas do repositório", zap.Error(err))
			span.SetStatus(codes.Error, "repository error")
			span.SetAttributes(attribute.Bool("error", true))
			return nil, err
		}

		// Armazenar no cache para futuras requisições
		if err := s.cache.Set(ctx, cacheKey, routes, 5*time.Minute); err != nil {
			s.logger.Warn("Erro ao armazenar rotas no cache", zap.Error(err))
		}
		span.SetAttributes(attribute.Bool("routes_list.from_cache", false))
	}

	// Registrar a quantidade de rotas encontradas
	span.SetAttributes(attribute.Int("routes.count", len(routes)))

	// Percorrer todas as rotas e verificar correspondência
	for _, r := range routes {
		if model.MatchRoutePath(r.Path, path) {
			s.logger.Info("Rota encontrada com correspondência de padrão",
				zap.String("registeredPath", r.Path),
				zap.String("requestPath", path),
				zap.String("serviceURL", r.ServiceURL))

			// Cache individual da rota para acesso mais rápido em requisições futuras
			routeCacheKey := "route:" + path
			if err := s.cache.Set(ctx, routeCacheKey, r, 5*time.Minute); err != nil {
				s.logger.Warn("Erro ao armazenar rota no cache", zap.Error(err))
			}

			// Adicionar informações de correspondência de padrões ao span
			span.SetAttributes(
				attribute.String("route.service_url", r.ServiceURL),
				attribute.Bool("route.is_active", r.IsActive),
				attribute.Bool("route.pattern_match", true),
				attribute.String("route.registered_path", r.Path),
			)
			span.SetStatus(codes.Ok, "rota encontrada por correspondência de padrões")

			return r, nil
		}
	}

	// Se não encontrou correspondência
	s.logger.Error("Nenhuma rota correspondente encontrada",
		zap.String("path", path))
	span.SetStatus(codes.Error, "rota não encontrada")
	return nil, repository.ErrRouteNotFound
}

// ClearCache limpa o cache de rotas
func (s *Service) ClearCache(ctx context.Context) error {
	// Limpar cache de rotas
	if err := s.cache.Delete(ctx, "routes"); err != nil {
		s.logger.Error("Erro ao limpar cache de rotas", zap.Error(err))
		return err
	}

	// Buscar todas as rotas para limpar cache individual
	routes, err := s.repo.GetRoutes(ctx)
	if err != nil {
		s.logger.Error("Erro ao buscar rotas para limpar cache", zap.Error(err))
		return err
	}

	for _, route := range routes {
		cacheKey := "route:" + route.Path
		if err := s.cache.Delete(ctx, cacheKey); err != nil {
			s.logger.Warn("Erro ao limpar cache de rota",
				zap.String("path", route.Path),
				zap.Error(err))
		}
	}

	s.logger.Info("Cache de rotas limpo com sucesso")
	return nil
}

// AddRoute adiciona uma nova rota
func (s *Service) AddRoute(ctx context.Context, route *model.Route) error {
	if err := s.repo.AddRoute(ctx, route); err != nil {
		return err
	}

	// Invalidar cache de rotas
	if err := s.cache.Delete(ctx, "routes"); err != nil {
		s.logger.Warn("Erro ao invalidar cache de rotas", zap.Error(err))
	}

	return nil
}

// UpdateRoute atualiza uma rota existente
func (s *Service) UpdateRoute(ctx context.Context, route *model.Route) error {
	if err := s.repo.UpdateRoute(ctx, route); err != nil {
		return err
	}

	// Invalidar caches
	cacheKey := "route:" + route.Path
	if err := s.cache.Delete(ctx, cacheKey); err != nil {
		s.logger.Warn("Erro ao invalidar cache de rota", zap.Error(err))
	}

	if err := s.cache.Delete(ctx, "routes"); err != nil {
		s.logger.Warn("Erro ao invalidar cache de rotas", zap.Error(err))
	}

	return nil
}

// DeleteRoute remove uma rota
func (s *Service) DeleteRoute(ctx context.Context, path string) error {
	if err := s.repo.DeleteRoute(ctx, path); err != nil {
		return err
	}

	// Invalidar caches
	cacheKey := "route:" + path
	if err := s.cache.Delete(ctx, cacheKey); err != nil {
		s.logger.Warn("Erro ao invalidar cache de rota", zap.Error(err))
	}

	if err := s.cache.Delete(ctx, "routes"); err != nil {
		s.logger.Warn("Erro ao invalidar cache de rotas", zap.Error(err))
	}

	return nil
}

// UpdateMetrics atualiza as métricas de uma rota
func (s *Service) UpdateMetrics(ctx context.Context, path string, callCount int64, totalResponseTime int64) error {
	return s.repo.UpdateMetrics(ctx, path, callCount, totalResponseTime)
}

// IsMethodAllowed verifica se um método é permitido para uma rota
func (s *Service) IsMethodAllowed(ctx context.Context, path, method string) (bool, error) {
	route, err := s.GetRouteByPath(ctx, path)
	if err != nil {
		return false, err
	}

	for _, m := range route.Methods {
		if m == method {
			return true, nil
		}
	}

	return false, nil
}
