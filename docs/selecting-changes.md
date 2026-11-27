# Selecting changes

The diff page's **changes** menu lists the first-parent commits since the merge
base with remote main (or local main when no remote main exists), oldest first,
and then the working tree.
A merge commit is one change relative to its first parent. Select any combination,
then choose **apply**. **All** and **none** select or clear the complete list.
All changes are selected by default. Picker choices are remembered in this browser
for each repository or worktree separately. New commits start selected. Selections
also stay in the page URL, including an explicitly empty selection; an explicit
URL selection takes precedence over saved choices. The old diff `mode` parameter
is no longer used.

The combined diff starts at the parent of the oldest selected commit and applies
only selected changes in chronological order, with working-tree changes last.
Working-tree changes include staged, unstaged, and untracked files. If omitted
commits are required to apply a selected change, the page reports a conflict;
select the prerequisite commits to resolve it. The real index is not changed.

The diff page shows the full messages of the selected commits, oldest first, in
a **commit messages** section before the files. The text of each commit in the
**changes** menu, and the id of each commit in that section, links to the commit
view, `?commit=<sha>`. The commit view shows only that commit: its message and
its diff. It does not change the saved picker choices, and **apply** goes back
to the normal diff page.

Each message in the **commit messages** section has an **edit** button. Save
changes the message of the commit, and makes the commits after it again on top
of it, with the same trees and messages. The index and the working tree do not
change. When the working tree is selected and has changes, it has an entry in
the section too: its edit button commits all of its changes, including files
that are not tracked, with the message and `git commit -s`.

The home page shows totals for all branch and working-tree changes, relative to
the same merge base as the default diff. It shows these totals only for worktrees
and for the repos that `-develop-on-main` names. These totals and the working-tree index
are cached. Saved picker choices do not change the home-page totals. Arbitrary
combinations are computed on request in temporary indexes and are not retained
or precomputed.
