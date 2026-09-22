package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/define42/GitOneS3/internal/repository"
	"github.com/define42/GitOneS3/internal/storage"
)

type repositoriesInput struct {
	Namespace string `path:"namespace" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"`
}

type createRepositoryInput struct {
	Namespace string `path:"namespace" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"`
	Body      struct {
		Name             string `json:"name" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$"`
		Description      string `json:"description,omitempty" maxLength:"500"`
		DefaultBranch    string `json:"defaultBranch,omitempty" default:"main" minLength:"1" maxLength:"128"`
		InitializeReadme bool   `json:"initializeReadme,omitempty"`
	}
}

type repositoryInput struct {
	Namespace  string `path:"namespace" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"`
	Repository string `path:"repository" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$"`
	Ref        string `query:"ref" maxLength:"256"`
	Path       string `query:"path" maxLength:"4096"`
}

type repositoryView struct {
	repository.Metadata
	Role     string `json:"role" enum:"reader,developer,owner"`
	CanWrite bool   `json:"canWrite"`
}

type repositoriesOutput struct {
	Body struct {
		Repositories []repositoryView `json:"repositories"`
		Role         string           `json:"role" enum:"reader,developer,owner"`
		CanWrite     bool             `json:"canWrite"`
	}
}

type repositoryOutput struct {
	Location string `header:"Location"`
	Body     repositoryView
}

type branchesOutput struct {
	Body struct {
		Branches []repository.Branch `json:"branches"`
	}
}

type treeOutput struct{ Body repository.Tree }
type blobOutput struct{ Body repository.Blob }
type commitsOutput struct {
	Body struct {
		Commits []repository.Commit `json:"commits"`
	}
}

func (s *Service) registerRepositoryAPI(api huma.API) {
	registerAPI(api, "list-repositories", "GET", "/api/v1/repos/{namespace}", "List private repositories in a space", false, http.StatusOK, s.apiRepositories)
	registerAPI(api, "create-repository", "POST", "/api/v1/repos/{namespace}", "Create a private repository", false, http.StatusCreated, s.apiCreateRepository)
	registerAPI(api, "get-repository", "GET", "/api/v1/repos/{namespace}/{repository}", "Get a private repository", false, http.StatusOK, s.apiRepository)
	registerAPI(api, "list-branches", "GET", "/api/v1/repos/{namespace}/{repository}/branches", "List repository branches", false, http.StatusOK, s.apiBranches)
	registerAPI(api, "get-tree", "GET", "/api/v1/repos/{namespace}/{repository}/tree", "Browse a repository directory", false, http.StatusOK, s.apiTree)
	registerAPI(api, "get-blob", "GET", "/api/v1/repos/{namespace}/{repository}/blob", "View a repository file", false, http.StatusOK, s.apiBlob)
	registerAPI(api, "list-commits", "GET", "/api/v1/repos/{namespace}/{repository}/commits", "List repository commit history", false, http.StatusOK, s.apiCommits)
}

// Read the authoritative namespace on every request: invitations are not
// membership, and a revoked role must not survive in cookies or repository data.
func (s *Service) repositoryRole(ctx context.Context, namespace string, write bool) (string, error) {
	current := currentAPISession(ctx).current
	record, _, err := s.loadNamespace(ctx, namespace)
	if errors.Is(err, storage.ErrNotFound) {
		return "", huma.Error404NotFound("space not found")
	}
	if err != nil {
		return "", huma.Error503ServiceUnavailable("space unavailable")
	}
	role := ""
	if record.Type == userNamespace && current.Username == namespace && userID(record.Identity) == userID(current.Identity) {
		role = "owner"
	}
	if record.Type == groupNamespace {
		role = record.Members[userID(current.Identity)]
	}
	if role == "" || (write && !canWriteRepositories(role)) {
		return "", huma.Error403Forbidden("you do not have access to this repository operation")
	}
	return role, nil
}

func canWriteRepositories(role string) bool { return role == "owner" || role == "developer" }

func (s *Service) apiRepositories(ctx context.Context, input *repositoriesInput) (*repositoriesOutput, error) {
	role, err := s.repositoryRole(ctx, input.Namespace, false)
	if err != nil {
		return nil, err
	}
	repositories, err := s.repositories.List(ctx, input.Namespace)
	if err != nil {
		return nil, repositoryAPIError(err)
	}
	output := &repositoriesOutput{}
	output.Body.Role, output.Body.CanWrite = role, canWriteRepositories(role)
	output.Body.Repositories = make([]repositoryView, 0, len(repositories))
	for _, metadata := range repositories {
		output.Body.Repositories = append(output.Body.Repositories, repositoryView{metadata, role, canWriteRepositories(role)})
	}
	return output, nil
}

func (s *Service) apiCreateRepository(ctx context.Context, input *createRepositoryInput) (*repositoryOutput, error) {
	role, err := s.repositoryRole(ctx, input.Namespace, true)
	if err != nil {
		return nil, err
	}
	current := currentAPISession(ctx).current
	metadata, err := s.repositories.Create(ctx, input.Namespace, repository.CreateInput{
		Name: input.Body.Name, Description: input.Body.Description, DefaultBranch: input.Body.DefaultBranch,
		InitializeReadme: input.Body.InitializeReadme, CreatedBy: userID(current.Identity),
		// Initial commits do not disclose the provider's verified email to other
		// group members. The chosen GitOne username is the public author name.
		AuthorName: current.Username, AuthorEmail: current.Username + "@users.gitone.invalid",
	})
	if err != nil {
		return nil, repositoryAPIError(err)
	}
	return &repositoryOutput{Location: "/" + input.Namespace + "/" + metadata.Name,
		Body: repositoryView{metadata, role, true}}, nil
}

func (s *Service) apiRepository(ctx context.Context, input *repositoryInput) (*repositoryOutput, error) {
	role, err := s.repositoryRole(ctx, input.Namespace, false)
	if err != nil {
		return nil, err
	}
	metadata, err := s.repositories.Get(ctx, input.Namespace, input.Repository)
	if err != nil {
		return nil, repositoryAPIError(err)
	}
	return &repositoryOutput{Body: repositoryView{metadata, role, canWriteRepositories(role)}}, nil
}

func (s *Service) apiBranches(ctx context.Context, input *repositoryInput) (*branchesOutput, error) {
	if _, err := s.repositoryRole(ctx, input.Namespace, false); err != nil {
		return nil, err
	}
	branches, err := s.repositories.Branches(ctx, input.Namespace, input.Repository)
	if err != nil {
		return nil, repositoryAPIError(err)
	}
	output := &branchesOutput{}
	output.Body.Branches = branches
	return output, nil
}

func (s *Service) apiTree(ctx context.Context, input *repositoryInput) (*treeOutput, error) {
	if _, err := s.repositoryRole(ctx, input.Namespace, false); err != nil {
		return nil, err
	}
	tree, err := s.repositories.Tree(ctx, input.Namespace, input.Repository, input.Ref, input.Path)
	if err != nil {
		return nil, repositoryAPIError(err)
	}
	return &treeOutput{Body: tree}, nil
}

func (s *Service) apiBlob(ctx context.Context, input *repositoryInput) (*blobOutput, error) {
	if _, err := s.repositoryRole(ctx, input.Namespace, false); err != nil {
		return nil, err
	}
	blob, err := s.repositories.Blob(ctx, input.Namespace, input.Repository, input.Ref, input.Path)
	if err != nil {
		return nil, repositoryAPIError(err)
	}
	return &blobOutput{Body: blob}, nil
}

func (s *Service) apiCommits(ctx context.Context, input *repositoryInput) (*commitsOutput, error) {
	if _, err := s.repositoryRole(ctx, input.Namespace, false); err != nil {
		return nil, err
	}
	commits, err := s.repositories.Commits(ctx, input.Namespace, input.Repository, input.Ref)
	if err != nil {
		return nil, repositoryAPIError(err)
	}
	output := &commitsOutput{}
	output.Body.Commits = commits
	return output, nil
}

func repositoryAPIError(err error) error {
	switch {
	case errors.Is(err, repository.ErrAlreadyExists):
		return huma.Error409Conflict("a repository with this name already exists in this space")
	case errors.Is(err, repository.ErrInvalid):
		return huma.Error400BadRequest("invalid repository name, description, branch, or file path")
	case errors.Is(err, repository.ErrNotFound):
		return huma.Error404NotFound("repository, branch, or file not found")
	default:
		return huma.Error503ServiceUnavailable("repository unavailable")
	}
}
