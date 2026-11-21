package main

import (
	"context"
	"log"
)

// sessionLabel returns the label of agent session n of a checkout: the
// agent name, then its working directory from ~, for example
// "pi · ~/ai/wt/corp/safer-ciguestlet-shutdown". For n > 1 the agent
// name has the session tag after it, for example "pi #2 · ~/ai/webdiff",
// so the sessions of one checkout have different labels. The home page
// shows the label on the agent row, and the Rex apps show it as the
// label of the agent window. Rex gets the label when the session starts,
// and again when a worktree rename moves the directory.
func sessionLabel(agent, dir string, n int) string {
	dir = tildePath(dir)
	if tag := sessionTag(n); tag != "" {
		if agent == "" {
			agent = tag
		} else {
			agent += " " + tag
		}
	}
	switch {
	case agent == "":
		return dir
	case dir == "":
		return agent
	default:
		return agent + " · " + dir
	}
}

// relabelAgentWindow sets the agent window label of the running Rex
// session to the sessionLabel for dir. The worktree rename calls it when
// the directory moves. New sessions get the label from
// createAgentSession. It does nothing for a tmux session, and only logs a
// failure: the label is not worth a failed rename.
func relabelAgentWindow(ctx context.Context, session, dir string) {
	s, ok := rexc.sessionByLabel(session)
	if !ok {
		return
	}
	label := sessionLabel(agentFromLabel(s.AgentLabel), dir, sessionNumber(session))
	if err := rexc.setAgentWindowLabel(ctx, s, label); err != nil {
		log.Printf("rex window label %s: %v", session, err)
	}
}
