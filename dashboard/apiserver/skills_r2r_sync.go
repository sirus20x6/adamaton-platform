// DEPRECATED: part of the evo dashboard, scheduled for harvest + removal.
// The deepresearch frontend at /thearray/git/deepresearch/platform/frontend/
// is the platform UI going forward. Pieces will be salvaged (Memory page
// already ported); the rest will be deleted. Do not extend this file --
// new dashboard work belongs in the deepresearch frontend / platform
// backend, not here.
//
package apiserver

import (
	"context"
	"os"
	"time"

	skillsworkflows "github.com/sirus20x6/adamomaton-knowledge/skills/workflows"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

// skillsR2RBaseURL is where the platform's skills ingest router lives.
// Defaults to the production Pi; override with SKILLS_R2R_BASE_URL for
// local testing. Kept here so other callers in the dashboard package
// (and the skills worker, via env) read the same source of truth even
// though the POST itself now happens inside an activity.
func skillsR2RBaseURL() string {
	if v := os.Getenv("SKILLS_R2R_BASE_URL"); v != "" {
		return v
	}
	return "https://deepresearch.local"
}

// skillsR2RCorpusID is the R2R collection a mirrored skill should be
// attached to (the "Skills" corpus, bootstrapped once via the platform
// corpora API). When unset the platform still ingests the document
// but it won't be filtered into a dedicated collection.
func skillsR2RCorpusID() string {
	return os.Getenv("SKILLS_R2R_CORPUS_ID")
}

// skillsR2REnabled returns false when the integration has been
// explicitly disabled (e.g. for local dev when the platform isn't
// reachable). Defaults to true so production is opt-out, not opt-in.
func skillsR2REnabled() bool {
	v := os.Getenv("SKILLS_R2R_ENABLED")
	if v == "" {
		return true
	}
	return v != "0" && v != "false" && v != "no"
}

// skillsR2RInsecure controls TLS verification when posting to the
// platform. The Pi serves a self-signed Caddy cert that the workstation
// already trusts via its CA bundle, but local dev against a fresh box
// often skips that step — set SKILLS_R2R_INSECURE=1 to bypass.
func skillsR2RInsecure() bool {
	v := os.Getenv("SKILLS_R2R_INSECURE")
	return v == "1" || v == "true" || v == "yes"
}

// syncSkillToR2R starts a SyncSkillToR2RWorkflow. Idempotent on
// skill_id (deterministic workflow ID), so re-syncing an in-flight
// skill cancels-and-restarts via WorkflowIDReusePolicy_TERMINATE_IF_RUNNING.
// Fire-and-forget at the HTTP layer — the workflow is durable, so the
// handler returning 201 immediately is safe. A failure to start the
// workflow (Temporal unreachable, etc.) is logged and swallowed: the
// canonical evo.skills row is already written, and a future re-save
// will retry the sync.
func (s *APIServer) syncSkillToR2R(sk Skill) {
	if !skillsR2REnabled() || s.temporalClient == nil {
		return
	}
	workflowID := "skill-sync-" + sk.ID
	in := skillsworkflows.SyncSkillToR2RInput{
		SkillID: sk.ID,
		Skill:   toSkillsWorkflowSkill(sk),
	}
	opts := client.StartWorkflowOptions{
		ID:                    workflowID,
		TaskQueue:             skillsworkflows.TaskQueue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_TERMINATE_IF_RUNNING,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.temporalClient.ExecuteWorkflow(ctx, opts, skillsworkflows.WorkflowSyncSkillToR2R, in); err != nil {
		s.logger.WithError(err).WithField("skill_id", sk.ID).
			Warn("failed to start skill sync workflow; sync will not run")
	}
}

// deleteSkillFromR2R starts a DeleteSkillFromR2RWorkflow with the same
// "terminate-if-running" idempotency contract as syncSkillToR2R. Called
// from the DELETE /skills/{id} handler after the evo.skills row has
// already been deleted; the workflow only owns the R2R-side cleanup.
func (s *APIServer) deleteSkillFromR2R(id string) {
	if !skillsR2REnabled() || s.temporalClient == nil {
		return
	}
	workflowID := "skill-delete-" + id
	in := skillsworkflows.DeleteSkillFromR2RInput{
		SkillID:       id,
		R2RDocumentID: id,
	}
	opts := client.StartWorkflowOptions{
		ID:                    workflowID,
		TaskQueue:             skillsworkflows.TaskQueue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_TERMINATE_IF_RUNNING,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.temporalClient.ExecuteWorkflow(ctx, opts, skillsworkflows.WorkflowDeleteSkillFromR2R, in); err != nil {
		s.logger.WithError(err).WithField("skill_id", id).
			Warn("failed to start skill delete workflow; R2R mirror may retain a stale doc")
	}
}

// toSkillsWorkflowSkill copies the dashboard's Skill into the
// skills/workflows package's mirror type. Field-for-field shallow copy
// — both structs use the same JSON tags, so Temporal's argument
// encoder round-trips losslessly. Lives on the dashboard side so the
// workflows module doesn't need to import the apiserver package.
func toSkillsWorkflowSkill(sk Skill) skillsworkflows.Skill {
	return skillsworkflows.Skill{
		ID:              sk.ID,
		Name:            sk.Name,
		Description:     sk.Description,
		Body:            sk.Body,
		WhenToUse:       sk.WhenToUse,
		Example:         sk.Example,
		Community:       sk.Community,
		Tags:            sk.Tags,
		DependsOn:       sk.DependsOn,
		Origin:          sk.Origin,
		SourceURL:       sk.SourceURL,
		SourceSHA:       sk.SourceSHA,
		SourceCheckedAt: sk.SourceCheckedAt,
		R2RDocumentID:   sk.R2RDocumentID,
		R2RCorpusID:     sk.R2RCorpusID,
		CreatedAt:       sk.CreatedAt,
		UpdatedAt:       sk.UpdatedAt,
	}
}