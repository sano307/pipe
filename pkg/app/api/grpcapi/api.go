// Copyright 2020 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package grpcapi

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pipe-cd/pipe/pkg/app/api/commandstore"
	"github.com/pipe-cd/pipe/pkg/app/api/service/apiservice"
	"github.com/pipe-cd/pipe/pkg/datastore"
	"github.com/pipe-cd/pipe/pkg/model"
	"github.com/pipe-cd/pipe/pkg/rpc/rpcauth"
)

// API implements the behaviors for the gRPC definitions of API.
type API struct {
	applicationStore datastore.ApplicationStore
	deploymentStore  datastore.DeploymentStore
	pipedStore       datastore.PipedStore
	commandStore     commandstore.Store

	logger *zap.Logger
}

// NewAPI creates a new API instance.
func NewAPI(
	ds datastore.DataStore,
	cmds commandstore.Store,
	logger *zap.Logger,
) *API {
	a := &API{
		applicationStore: datastore.NewApplicationStore(ds),
		deploymentStore:  datastore.NewDeploymentStore(ds),
		pipedStore:       datastore.NewPipedStore(ds),
		commandStore:     cmds,
		logger:           logger.Named("api"),
	}
	return a
}

// Register registers all handling of this service into the specified gRPC server.
func (a *API) Register(server *grpc.Server) {
	apiservice.RegisterAPIServiceServer(server, a)
}

func (a *API) AddApplication(ctx context.Context, req *apiservice.AddApplicationRequest) (*apiservice.AddApplicationResponse, error) {
	key, err := requireAPIKey(ctx, model.APIKey_READ_WRITE, a.logger)
	if err != nil {
		return nil, err
	}

	piped, err := getPiped(ctx, a.pipedStore, req.PipedId, a.logger)
	if err != nil {
		return nil, err
	}

	if key.ProjectId != piped.ProjectId {
		return nil, status.Error(codes.InvalidArgument, "Requested piped does not belong to your project")
	}

	gitpath, err := makeGitPath(
		req.GitPath.Repo.Id,
		req.GitPath.Path,
		req.GitPath.ConfigFilename,
		piped,
		a.logger,
	)
	if err != nil {
		return nil, err
	}

	app := model.Application{
		Id:            uuid.New().String(),
		Name:          req.Name,
		EnvId:         req.EnvId,
		PipedId:       req.PipedId,
		ProjectId:     key.ProjectId,
		GitPath:       gitpath,
		Kind:          req.Kind,
		CloudProvider: req.CloudProvider,
	}
	err = a.applicationStore.AddApplication(ctx, &app)
	if errors.Is(err, datastore.ErrAlreadyExists) {
		return nil, status.Error(codes.AlreadyExists, "The application already exists")
	}
	if err != nil {
		a.logger.Error("failed to create application", zap.Error(err))
		return nil, status.Error(codes.Internal, "Failed to create application")
	}

	return &apiservice.AddApplicationResponse{
		ApplicationId: app.Id,
	}, nil
}

func (a *API) SyncApplication(ctx context.Context, req *apiservice.SyncApplicationRequest) (*apiservice.SyncApplicationResponse, error) {
	key, err := requireAPIKey(ctx, model.APIKey_READ_WRITE, a.logger)
	if err != nil {
		return nil, err
	}

	app, err := getApplication(ctx, a.applicationStore, req.ApplicationId, a.logger)
	if err != nil {
		return nil, err
	}

	if key.ProjectId != app.ProjectId {
		return nil, status.Error(codes.InvalidArgument, "Requested application does not belong to your project")
	}

	cmd := model.Command{
		Id:            uuid.New().String(),
		PipedId:       app.PipedId,
		ApplicationId: app.Id,
		Type:          model.Command_SYNC_APPLICATION,
		Commander:     key.Id,
		SyncApplication: &model.Command_SyncApplication{
			ApplicationId: app.Id,
			SyncStrategy:  model.SyncStrategy_AUTO,
		},
	}
	if err := addCommand(ctx, a.commandStore, &cmd, a.logger); err != nil {
		return nil, err
	}

	return &apiservice.SyncApplicationResponse{
		CommandId: cmd.Id,
	}, nil
}

func (a *API) GetDeployment(ctx context.Context, req *apiservice.GetDeploymentRequest) (*apiservice.GetDeploymentResponse, error) {
	key, err := requireAPIKey(ctx, model.APIKey_READ_ONLY, a.logger)
	if err != nil {
		return nil, err
	}

	deployment, err := getDeployment(ctx, a.deploymentStore, req.DeploymentId, a.logger)
	if err != nil {
		return nil, err
	}

	if key.ProjectId != deployment.ProjectId {
		return nil, status.Error(codes.InvalidArgument, "Requested deployment does not belong to your project")
	}

	return &apiservice.GetDeploymentResponse{
		Deployment: deployment,
	}, nil
}

func (a *API) GetCommand(ctx context.Context, req *apiservice.GetCommandRequest) (*apiservice.GetCommandResponse, error) {
	_, err := requireAPIKey(ctx, model.APIKey_READ_ONLY, a.logger)
	if err != nil {
		return nil, err
	}

	cmd, err := getCommand(ctx, a.commandStore, req.CommandId, a.logger)
	if err != nil {
		return nil, err
	}

	return &apiservice.GetCommandResponse{
		Command: cmd,
	}, nil
}

// requireAPIKey checks the existence of an API key inside the given context
// and ensures that it has enough permissions for the give role.
func requireAPIKey(ctx context.Context, role model.APIKey_Role, logger *zap.Logger) (*model.APIKey, error) {
	key, err := rpcauth.ExtractAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	switch key.Role {
	case model.APIKey_READ_WRITE:
		return key, nil

	case model.APIKey_READ_ONLY:
		if role == model.APIKey_READ_ONLY {
			return key, nil
		}
		logger.Warn("detected an API key that has insufficient permissions", zap.String("key", key.Id))
		return nil, status.Error(codes.PermissionDenied, "Permission denied")

	default:
		logger.Warn("detected an API key that has an invalid role", zap.String("key", key.Id))
		return nil, status.Error(codes.PermissionDenied, "Invalid role")
	}
}
