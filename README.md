# webdiff

A mobile-friendly web UI served over Tailscale for reviewing git diffs directly on your LLM sandbox, with an integrated Claude code-review workflow. It has 3 main views:

* Home page listing git repos and agent sessions
* Diff page showing git diff output, with the ability to add comments and send them to claude
* Terminal page showing a stream of the claude session for a particular repo, with some minimal controls for interactivity

Experimental, mostly LLM-written, and highly personal to my workflow for now.
