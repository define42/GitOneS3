package auth

import (
	"context"

	"github.com/define42/GitOneS3/internal/repository"
)

type browseInput struct {
	Namespace  string `path:"namespace" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$"`
	Repository string `path:"repository" minLength:"1" maxLength:"63" pattern:"^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$"`
	Ref        string `query:"ref" maxLength:"256"`
	Path       string `query:"path" maxLength:"4096"`
	View       string `query:"view" enum:"commits"`
}

type browseOutput struct {
	Body struct {
		Repository repositoryView      `json:"repository"`
		Branches   []repository.Branch `json:"branches"`
		Tree       *repository.Tree    `json:"tree,omitempty"`
		Blob       *repository.Blob    `json:"blob,omitempty"`
		Readme     *repository.Blob    `json:"readme,omitempty"`
		Commits    []repository.Commit `json:"commits,omitempty"`
	}
}

func (s *Service) apiBrowse(ctx context.Context, input *browseInput) (*browseOutput, error) {
	role, err := s.repositoryRole(ctx, input.Namespace, false)
	if err != nil {
		return nil, err
	}
	release, err := s.acquireBrowser(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	page, err := s.repositories.Browse(ctx, input.Namespace, input.Repository, input.Ref, input.Path, input.View == "commits")
	if err != nil {
		return nil, repositoryAPIError(err)
	}
	output := &browseOutput{}
	output.Body.Repository = repositoryView{page.Metadata, role, canWriteRepositories(role)}
	output.Body.Branches = page.Branches
	output.Body.Tree, output.Body.Blob, output.Body.Readme = page.Tree, page.Blob, page.Readme
	output.Body.Commits = page.Commits
	return output, nil
}
