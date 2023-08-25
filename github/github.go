package github

import (
	"context"
	"fmt"
	configuration "github.com/diggerhq/lib-digger-config"
	orchestrator "github.com/diggerhq/lib-orchestrator"
	"github.com/diggerhq/lib-orchestrator/github/models"
	"log"
	"strings"

	"github.com/google/go-github/v53/github"
)

func NewGitHubService(ghToken string, repoName string, owner string) GithubService {
	client := github.NewTokenClient(context.Background(), ghToken)
	return GithubService{
		Client:   client,
		RepoName: repoName,
		Owner:    owner,
	}
}

type GithubService struct {
	Client   *github.Client
	RepoName string
	Owner    string
}

func (svc *GithubService) GetUserTeams(organisation string, user string) ([]string, error) {
	teamsResponse, _, err := svc.Client.Teams.ListTeams(context.Background(), organisation, nil)
	if err != nil {
		log.Fatalf("Failed to list github teams: %v", err)
	}
	var teams []string
	for _, team := range teamsResponse {
		teamMembers, _, _ := svc.Client.Teams.ListTeamMembersBySlug(context.Background(), organisation, *team.Slug, nil)
		for _, member := range teamMembers {
			if *member.Login == user {
				teams = append(teams, *team.Name)
				break
			}
		}
	}

	return teams, nil
}

func (svc *GithubService) GetChangedFiles(prNumber int) ([]string, error) {
	files, _, err := svc.Client.PullRequests.ListFiles(context.Background(), svc.Owner, svc.RepoName, prNumber, nil)
	if err != nil {
		log.Fatalf("error getting pull request files: %v", err)
	}

	fileNames := make([]string, len(files))

	for i, file := range files {
		fileNames[i] = *file.Filename
	}
	return fileNames, nil
}

func (svc *GithubService) PublishComment(prNumber int, comment string) error {
	_, _, err := svc.Client.Issues.CreateComment(context.Background(), svc.Owner, svc.RepoName, prNumber, &github.IssueComment{Body: &comment})
	return err
}

func (svc *GithubService) GetComments(prNumber int) ([]orchestrator.Comment, error) {
	comments, _, err := svc.Client.Issues.ListComments(context.Background(), svc.Owner, svc.RepoName, prNumber, &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}})
	commentBodies := make([]orchestrator.Comment, len(comments))
	for i, comment := range comments {
		commentBodies[i] = orchestrator.Comment{
			Id:   *comment.ID,
			Body: comment.Body,
		}
	}
	return commentBodies, err
}

func (svc *GithubService) EditComment(id interface{}, comment string) error {
	commentId := id.(int64)
	_, _, err := svc.Client.Issues.EditComment(context.Background(), svc.Owner, svc.RepoName, commentId, &github.IssueComment{Body: &comment})
	return err
}

func (svc *GithubService) SetStatus(prNumber int, status string, statusContext string) error {
	pr, _, err := svc.Client.PullRequests.Get(context.Background(), svc.Owner, svc.RepoName, prNumber)
	if err != nil {
		log.Fatalf("error getting pull request: %v", err)
	}

	_, _, err = svc.Client.Repositories.CreateStatus(context.Background(), svc.Owner, svc.RepoName, *pr.Head.SHA, &github.RepoStatus{
		State:       &status,
		Context:     &statusContext,
		Description: &statusContext,
	})
	return err
}

func (svc *GithubService) GetCombinedPullRequestStatus(prNumber int) (string, error) {
	pr, _, err := svc.Client.PullRequests.Get(context.Background(), svc.Owner, svc.RepoName, prNumber)
	if err != nil {
		log.Fatalf("error getting pull request: %v", err)
	}

	statuses, _, err := svc.Client.Repositories.GetCombinedStatus(context.Background(), svc.Owner, svc.RepoName, pr.Head.GetSHA(), nil)
	if err != nil {
		log.Fatalf("error getting combined status: %v", err)
	}

	return *statuses.State, nil
}

func (svc *GithubService) MergePullRequest(prNumber int) error {
	pr, _, err := svc.Client.PullRequests.Get(context.Background(), svc.Owner, svc.RepoName, prNumber)
	if err != nil {
		log.Fatalf("error getting pull request: %v", err)
	}

	_, _, err = svc.Client.PullRequests.Merge(context.Background(), svc.Owner, svc.RepoName, prNumber, "auto-merge", &github.PullRequestOptions{
		MergeMethod: "squash",
		SHA:         pr.Head.GetSHA(),
	})
	return err
}

func isMergeableState(mergeableState string) bool {
	// https://docs.github.com/en/github-ae@latest/graphql/reference/enums#mergestatestatus
	mergeableStates := map[string]int{
		"clean":     0,
		"unstable":  0,
		"has_hooks": 1,
	}
	_, exists := mergeableStates[strings.ToLower(mergeableState)]
	if !exists {
		log.Printf("pr.GetMergeableState() returned: %v", mergeableState)
	}

	return exists
}

func (svc *GithubService) IsMergeable(prNumber int) (bool, error) {
	pr, _, err := svc.Client.PullRequests.Get(context.Background(), svc.Owner, svc.RepoName, prNumber)
	if err != nil {
		log.Fatalf("error getting pull request: %v", err)
		return false, err
	}

	return pr.GetMergeable() && isMergeableState(pr.GetMergeableState()), nil
}

func (svc *GithubService) IsMerged(prNumber int) (bool, error) {
	pr, _, err := svc.Client.PullRequests.Get(context.Background(), svc.Owner, svc.RepoName, prNumber)
	if err != nil {
		log.Fatalf("error getting pull request: %v", err)
		return false, err
	}
	return *pr.Merged, nil
}

func (svc *GithubService) IsClosed(prNumber int) (bool, error) {
	pr, _, err := svc.Client.PullRequests.Get(context.Background(), svc.Owner, svc.RepoName, prNumber)
	if err != nil {
		log.Fatalf("error getting pull request: %v", err)
		return false, err
	}

	return pr.GetState() == "closed", nil
}

func ConvertGithubEventToJobs(parsedGhContext models.EventPackage, impactedProjects []configuration.Project, requestedProject *configuration.Project, workflows map[string]configuration.Workflow) ([]orchestrator.Job, bool, error) {
	jobs := make([]orchestrator.Job, 0)

	switch event := parsedGhContext.Event.(type) {
	case github.PullRequestEvent:
		for _, project := range impactedProjects {
			workflow, ok := workflows[project.Workflow]
			if !ok {
				return nil, false, fmt.Errorf("failed to find workflow config '%s' for project '%s'", project.Workflow, project.Name)
			}

			stateEnvVars, commandEnvVars := configuration.CollectTerraformEnvConfig(workflow.EnvVars)

			if event.GetAction() == "closed" && *event.PullRequest.Merged && event.PullRequest.Base.Ref == event.Repo.DefaultBranch {
				jobs = append(jobs, orchestrator.Job{
					ProjectName:       project.Name,
					ProjectDir:        project.Dir,
					ProjectWorkspace:  project.Workspace,
					Terragrunt:        project.Terragrunt,
					Commands:          workflow.Configuration.OnCommitToDefault,
					ApplyStage:        orchestrator.ToConfigStage(workflow.Apply),
					PlanStage:         orchestrator.ToConfigStage(workflow.Plan),
					CommandEnvVars:    commandEnvVars,
					StateEnvVars:      stateEnvVars,
					PullRequestNumber: event.PullRequest.Number,
					EventName:         "pull_request",
					RequestedBy:       parsedGhContext.Actor,
					Namespace:         parsedGhContext.Repository,
				})
			} else if *event.Action == "opened" || *event.Action == "reopened" || *event.Action == "synchronize" {
				jobs = append(jobs, orchestrator.Job{
					ProjectName:       project.Name,
					ProjectDir:        project.Dir,
					ProjectWorkspace:  project.Workspace,
					Terragrunt:        project.Terragrunt,
					Commands:          workflow.Configuration.OnPullRequestPushed,
					ApplyStage:        orchestrator.ToConfigStage(workflow.Apply),
					PlanStage:         orchestrator.ToConfigStage(workflow.Plan),
					CommandEnvVars:    commandEnvVars,
					StateEnvVars:      stateEnvVars,
					PullRequestNumber: event.PullRequest.Number,
					EventName:         "pull_request",
					Namespace:         parsedGhContext.Repository,
					RequestedBy:       parsedGhContext.Actor,
				})
			} else if *event.Action == "closed" {
				jobs = append(jobs, orchestrator.Job{
					ProjectName:       project.Name,
					ProjectDir:        project.Dir,
					ProjectWorkspace:  project.Workspace,
					Terragrunt:        project.Terragrunt,
					Commands:          workflow.Configuration.OnPullRequestClosed,
					ApplyStage:        orchestrator.ToConfigStage(workflow.Apply),
					PlanStage:         orchestrator.ToConfigStage(workflow.Plan),
					CommandEnvVars:    commandEnvVars,
					StateEnvVars:      stateEnvVars,
					PullRequestNumber: event.PullRequest.Number,
					EventName:         "pull_request",
					Namespace:         parsedGhContext.Repository,
					RequestedBy:       parsedGhContext.Actor,
				})
			}
		}
		return jobs, true, nil
	case github.IssueCommentEvent:
		supportedCommands := []string{"digger plan", "digger apply", "digger unlock", "digger lock"}

		coversAllImpactedProjects := true

		runForProjects := impactedProjects

		if requestedProject != nil {
			if len(impactedProjects) > 1 {
				coversAllImpactedProjects = false
				runForProjects = []configuration.Project{*requestedProject}
			} else if len(impactedProjects) == 1 && impactedProjects[0].Name != requestedProject.Name {
				return jobs, false, fmt.Errorf("requested project %v is not impacted by this PR", requestedProject.Name)
			}
		}

		diggerCommand := strings.ToLower(*event.Comment.Body)
		diggerCommand = strings.TrimSpace(diggerCommand)

		for _, command := range supportedCommands {
			if strings.HasPrefix(diggerCommand, command) {
				for _, project := range runForProjects {
					workflow, ok := workflows[project.Workflow]
					if !ok {
						return nil, false, fmt.Errorf("failed to find workflow config '%s' for project '%s'", project.Workflow, project.Name)
					}

					stateEnvVars, commandEnvVars := configuration.CollectTerraformEnvConfig(workflow.EnvVars)

					workspace := project.Workspace
					workspaceOverride, err := orchestrator.ParseWorkspace(*event.Comment.Body)
					if err != nil {
						return []orchestrator.Job{}, false, err
					}
					if workspaceOverride != "" {
						workspace = workspaceOverride
					}
					jobs = append(jobs, orchestrator.Job{
						ProjectName:       project.Name,
						ProjectDir:        project.Dir,
						ProjectWorkspace:  workspace,
						Terragrunt:        project.Terragrunt,
						Commands:          []string{command},
						ApplyStage:        orchestrator.ToConfigStage(workflow.Apply),
						PlanStage:         orchestrator.ToConfigStage(workflow.Plan),
						CommandEnvVars:    commandEnvVars,
						StateEnvVars:      stateEnvVars,
						PullRequestNumber: event.Issue.Number,
						EventName:         "issue_comment",
						RequestedBy:       parsedGhContext.Actor,
						Namespace:         parsedGhContext.Repository,
					})
				}
			}
		}
		return jobs, coversAllImpactedProjects, nil
	default:
		return []orchestrator.Job{}, false, fmt.Errorf("unsupported event type: %T", parsedGhContext.EventName)
	}
}

func ProcessGitHubEvent(ghEvent interface{}, diggerConfig *configuration.DiggerConfig, ciService orchestrator.PullRequestService) ([]configuration.Project, *configuration.Project, int, error) {
	var impactedProjects []configuration.Project
	var prNumber int

	switch event := ghEvent.(type) {
	case github.PullRequestEvent:
		prNumber = *event.GetPullRequest().Number
		changedFiles, err := ciService.GetChangedFiles(prNumber)

		if err != nil {
			return nil, nil, 0, fmt.Errorf("could not get changed files")
		}

		impactedProjects = diggerConfig.GetModifiedProjects(changedFiles)
	case github.IssueCommentEvent:
		prNumber = *event.GetIssue().Number
		changedFiles, err := ciService.GetChangedFiles(prNumber)

		if err != nil {
			return nil, nil, 0, fmt.Errorf("could not get changed files")
		}

		impactedProjects = diggerConfig.GetModifiedProjects(changedFiles)
		requestedProject := orchestrator.ParseProjectName(*event.Comment.Body)

		if requestedProject == "" {
			return impactedProjects, nil, prNumber, nil
		}

		for _, project := range impactedProjects {
			if project.Name == requestedProject {
				return impactedProjects, &project, prNumber, nil
			}
		}
		return nil, nil, 0, fmt.Errorf("requested project not found in modified projects")

	default:
		return nil, nil, 0, fmt.Errorf("unsupported event type")
	}
	return impactedProjects, nil, prNumber, nil
}

func issueCommentEventContainsComment(event interface{}, comment string) bool {
	switch event.(type) {
	case github.IssueCommentEvent:
		event := event.(github.IssueCommentEvent)
		if strings.Contains(*event.Comment.Body, comment) {
			return true
		}
	}
	return false
}

func CheckIfHelpComment(event interface{}) bool {
	return issueCommentEventContainsComment(event, "digger help")
}
