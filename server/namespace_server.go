package server

import (
	"context"
	"math"
	"strings"

	"github.com/onepanelio/core/api"
	v1 "github.com/onepanelio/core/pkg"
	"github.com/onepanelio/core/server/auth"
)

type NamespaceServer struct{}

func NewNamespaceServer() *NamespaceServer {
	return &NamespaceServer{}
}

func apiNamespace(ns *v1.Namespace) (namespace *api.Namespace) {
	namespace = &api.Namespace{
		Name: ns.Name,
	}

	return
}

func (s *NamespaceServer) ListNamespaces(ctx context.Context, req *api.ListNamespacesRequest) (*api.ListNamespacesResponse, error) {
	client := ctx.Value("kubeClient").(*v1.Client)
	allowed, err := auth.IsAuthorized(client, "", "list", "", "namespaces", "")
	if err != nil || !allowed {
		return nil, err
	}

	if req.PageSize <= 0 {
		req.PageSize = 15
	}

	namespaces, err := client.ListNamespaces()
	if err != nil {
		return nil, err
	}

	var apiNamespaces []*api.Namespace
	for _, ns := range namespaces {
		if req.Query == "" || (req.Query != "" && strings.Contains(ns.Name, req.Query)) {
			apiNamespaces = append(apiNamespaces, apiNamespace(ns))
		}
	}

	pages := int32(math.Ceil(float64(len(apiNamespaces)) / float64(req.PageSize)))
	if req.Page > pages {
		req.Page = pages
	}

	if req.Page <= 0 {
		req.Page = 1
	}

	start := (req.Page - 1) * req.PageSize
	end := start + req.PageSize
	if end >= int32(len(apiNamespaces)) {
		end = int32(len(apiNamespaces))
	}

	return &api.ListNamespacesResponse{
		Count:      end - start,
		Namespaces: apiNamespaces[start:end],
		Page:       req.Page,
		Pages:      pages,
		TotalCount: int32(len(apiNamespaces)),
	}, nil
}

func (s *NamespaceServer) CreateNamespace(ctx context.Context, createNamespace *api.CreateNamespaceRequest) (*api.Namespace, error) {
	client := ctx.Value("kubeClient").(*v1.Client)
	allowed, err := auth.IsAuthorized(client, "", "create", "", "namespaces", "")
	if err != nil || !allowed {
		return nil, err
	}

	namespace, err := client.CreateNamespace(createNamespace.Namespace.Name)
	if err != nil {
		return nil, err
	}

	return &api.Namespace{
		Name: namespace.Name,
	}, nil
}
