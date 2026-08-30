#!/usr/bin/env bash
# One-time migration of managed worktrees from the old flat cache layout
#
#   ~/.cache/webdiff/worktrees/<repo>-<sanitizedBranch>
#
# to the nested layout next to the repos:
#
#   ~/ai/wt/<repo>/<leaf>
#
# The URL slug stays flat ("<repo>-<leaf>"), so most identity is preserved
# by construction — but the *session name* is derived from the slug, and a
# session name is baked into history ids, attachment dirs and the tmux
# session itself, all of which have to be renamed in step with the move.
#
# Idempotent: every step skips work that's already been done, so a partial
# run can be re-run. Run it with the new binary built but NOT yet deployed
# — the new code can't see the old layout, so the service is stopped for
# the duration.
set -euo pipefail

ai_root="$HOME/ai"
cache="$HOME/.cache/webdiff"
old_root="$cache/worktrees"
new_root="$ai_root/wt"
history_root="$cache/history"
attachments_root="$cache/attachments"
backup_root="$HOME/webdiff-wt-backup-$(date +%Y%m%d)"

log() { printf '%s\n' "$*" >&2; }
die() { log "FATAL: $*"; exit 1; }

command -v jq >/dev/null || die "jq is required to rewrite history metadata"

# leafOf strips the "<parent>-" prefix and then the owner prefix, matching
# what worktreeLeaf() in the Go code produces for a `tomhjp/<topic>`
# branch. Derived from the *existing directory name*, not from the branch:
# at least one worktree here sits on a branch that no longer matches its
# directory, and re-deriving from the branch would move it to a leaf whose
# slug doesn't match its live session or its history.
leafOf() {
	local parent=$1 dir=$2 leaf
	leaf=${dir#"$parent"-}
	[ "$leaf" != "$dir" ] || die "worktree dir $dir does not start with its parent repo $parent-"
	printf '%s' "${leaf#tomhjp-}"
}

# --- gather: parent repo per worktree, straight from git ---

declare -a old_dirs=() parents=() leaves=() new_slugs=()

if [ -d "$old_root" ]; then
	while IFS=$'\t' read -r parent abs; do
		dir=$(basename "$abs")
		leaf=$(leafOf "$parent" "$dir")
		[ -n "$leaf" ] || die "empty leaf for $abs"
		old_dirs+=("$dir")
		parents+=("$parent")
		leaves+=("$leaf")
		new_slugs+=("$parent-$leaf")
	done < <(
		cd "$ai_root"
		for r in */; do
			r=${r%/}
			if [ -d "$r/.git" ]; then
				git -C "$r" worktree list --porcelain 2>/dev/null |
					awk -v repo="$r" '/^worktree /{print repo"\t"$2}'
			fi
		done | grep -F "$old_root/" | sort -k2
	)
fi

n=${#old_dirs[@]}
log "found $n worktree(s) under $old_root"

# Any dir in old_root that git doesn't know about would be silently left
# behind, so refuse rather than half-migrate.
if [ -d "$old_root" ]; then
	for d in "$old_root"/*; do
		[ -d "$d" ] || continue
		b=$(basename "$d")
		found=0
		for ((i = 0; i < n; i++)); do
			if [ "${old_dirs[i]}" = "$b" ]; then
				found=1
				break
			fi
		done
		[ "$found" = 1 ] || die "$d is not a registered worktree of any repo in $ai_root"
	done
fi

# --- preconditions: unique slugs, no clash with a repo, no dest present ---

for ((i = 0; i < n; i++)); do
	for ((j = i + 1; j < n; j++)); do
		[ "${new_slugs[i]}" != "${new_slugs[j]}" ] ||
			die "slug collision: ${old_dirs[i]} and ${old_dirs[j]} both flatten to ${new_slugs[i]}"
	done
	# A worktree and a repo live in separate URL namespaces, but they'd
	# still share a tmux session name.
	[ ! -e "$ai_root/${new_slugs[i]}" ] ||
		die "slug ${new_slugs[i]} collides with $ai_root/${new_slugs[i]}"
	# git worktree move silently moves *into* an existing destination.
	dest="$new_root/${parents[i]}/${leaves[i]}"
	if [ -e "$dest" ] && [ -e "$old_root/${old_dirs[i]}" ]; then
		die "$dest already exists but source $old_root/${old_dirs[i]} is still there"
	fi
done
log "preconditions ok: $n unique slug(s), no destination in the way"

# --- 1. backup ---

if [ -e "$backup_root" ]; then
	log "backup $backup_root already exists — skipping"
elif [ ! -d "$old_root" ]; then
	log "no $old_root — nothing to back up"
else
	mkdir -p "$backup_root"
	# Hardlink snapshot: seconds and ~no extra disk, and it survives the
	# renames and deletes below because none of them rewrite file contents
	# in place. It is not a defence against an editor truncating a file,
	# but this migration never touches file contents.
	cp -al "$old_root" "$backup_root/tree"
	# The gitdirs on the repo end of each worktree link, plus the history
	# metas we rewrite, are small enough for a real copy.
	declare -a wt_meta=()
	for r in "$ai_root"/*/; do
		r=$(basename "${r%/}")
		if [ -d "$ai_root/$r/.git/worktrees" ]; then
			wt_meta+=("$r/.git/worktrees")
		fi
	done
	if [ ${#wt_meta[@]} -gt 0 ]; then
		tar czf "$backup_root/git-worktrees.tar.gz" -C "$ai_root" "${wt_meta[@]}"
	fi
	if [ -d "$history_root" ]; then
		tar czf "$backup_root/history.tar.gz" -C "$cache" history
	fi
	log "backed up to $backup_root"
fi

# --- 2. stop the service ---
#
# A running agent can mutate worktree state mid-move, and the old binary
# would keep writing history under the old session names.

stopped=0
if systemctl --user is-active --quiet webdiff; then
	log "stopping webdiff"
	systemctl --user stop webdiff
	stopped=1
fi

# --- 3. move the worktrees ---

mkdir -p "$new_root"
for ((i = 0; i < n; i++)); do
	src="$old_root/${old_dirs[i]}"
	dest="$new_root/${parents[i]}/${leaves[i]}"
	if [ ! -e "$src" ]; then
		log "  skip ${old_dirs[i]} (already moved)"
		continue
	fi
	# `git worktree move` uses a bare rename(2): the destination's parent
	# must exist, and the leaf must not.
	mkdir -p "$(dirname "$dest")"
	log "  ${old_dirs[i]} -> wt/${parents[i]}/${leaves[i]}"
	if ! git -C "$ai_root/${parents[i]}" worktree move "$src" "$dest"; then
		log "    git worktree move failed; falling back to mv + repair"
		mv "$src" "$dest"
		git -C "$ai_root/${parents[i]}" worktree repair "$dest"
	fi
done
rmdir "$old_root" 2>/dev/null || true

# --- 4. rename the live tmux sessions ---
#
# Panes keep running and an open cwd follows the rename; only each shell's
# own $PWD string stays stale until its next cd.

for ((i = 0; i < n; i++)); do
	old_sess="${old_dirs[i]}-wd"
	new_sess="${new_slugs[i]}-wd"
	[ "$old_sess" != "$new_sess" ] || continue
	if tmux has-session -t "=$old_sess" 2>/dev/null; then
		log "  tmux rename $old_sess -> $new_sess"
		tmux rename-session -t "=$old_sess" "$new_sess"
	fi
done

# --- 5. attachment dirs, keyed by session name ---

for ((i = 0; i < n; i++)); do
	src="$attachments_root/${old_dirs[i]}-wd"
	dest="$attachments_root/${new_slugs[i]}-wd"
	[ "$src" != "$dest" ] || continue
	if [ -d "$src" ] && [ ! -e "$dest" ]; then
		log "  attachments ${old_dirs[i]}-wd -> ${new_slugs[i]}-wd"
		mv "$src" "$dest"
	fi
done

# --- 6. history: rewrite name + id, and rename both files ---
#
# The id is `<started>-<session>` and newestArchiveIDs parses the
# *filename*, not the JSON, so a renamed session with un-renamed ids would
# show every migrated session as a stopped row, break resume, and split its
# scrollback. Restricted to the worktrees actually on disk: applying the
# same transform to every historical worktree meta collides, because
# long-removed worktrees can flatten onto a live slug.

declare -A slug_map=()
for ((i = 0; i < n; i++)); do
	[ "${old_dirs[i]}" != "${new_slugs[i]}" ] || continue
	slug_map["${old_dirs[i]}"]="${new_slugs[i]}"
done

if [ -d "$history_root" ] && [ ${#slug_map[@]} -gt 0 ]; then
	for meta in "$history_root"/*.json; do
		[ -f "$meta" ] || continue
		IFS=$'\t' read -r kind name old_id started < <(
			jq -r '[.kind // "", .name // "", .id, .started] | @tsv' "$meta"
		)
		[ "$kind" = worktrees ] || continue
		new_name=${slug_map["$name"]:-}
		[ -n "$new_name" ] || continue
		new_id="$started-$new_name-wd"
		[ "$old_id" != "$new_id" ] || continue
		[ ! -e "$history_root/$new_id.json" ] || die "history id collision: $new_id"
		log "  history $old_id -> $new_id"
		jq --arg id "$new_id" --arg name "$new_name" --arg sess "$new_name-wd" \
			'.id = $id | .name = $name | .session = $sess' "$meta" >"$meta.tmp"
		mv "$meta.tmp" "$history_root/$new_id.json"
		rm -f "$meta"
		if [ -f "$history_root/$old_id.ansi" ]; then
			mv "$history_root/$old_id.ansi" "$history_root/$new_id.ansi"
		fi
	done
fi

# --- 7. drop caches keyed by the old slug ---
#
# All rebuildable: the diff index, the sync read-tree index (keyed by a
# hash of the absolute path, so every worktree pays one rebuild), and a
# dead root from an older version.

for ((i = 0; i < n; i++)); do
	[ "${old_dirs[i]}" != "${new_slugs[i]}" ] || continue
	rm -rf "$cache/repos/${old_dirs[i]}"
done
rm -rf "$cache/sync-indexes" "$cache/home"

# --- 8. restart ---

if [ "$stopped" = 1 ]; then
	log "starting webdiff"
	systemctl --user start webdiff
fi

log "done. backup kept at $backup_root"
