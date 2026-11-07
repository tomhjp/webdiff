# terminal-to-html, patched for webdiff

This is github.com/buildkite/terminal-to-html/v3 at v3.17.3 with
`upstream-24-bit-colors.patch` applied, without the tests, the
command-line tools, and the assets. webdiff's go.mod points the module
here with a `replace` directive.

The patch: v3.17.3 parses 24-bit SGR colours (`38;2;r;g;b` and
`48;2;r;g;b`), but drops them from the HTML. The patch writes them as an
inline `style` on the span. Pi sends 24-bit colours when it runs in Rex,
so without the patch its tool-call backgrounds and syntax colours are
lost.

The patch file is in `git format-patch` form, made against the v3.17.3
tag, with the upstream tests updated, so it can go upstream as a PR. To
make this copy again:

    git clone --branch v3.17.3 https://github.com/buildkite/terminal-to-html
    cd terminal-to-html
    git am <webdiff>/third_party/terminal-to-html/upstream-24-bit-colors.patch
    go test .
    cp $(ls *.go | grep -v _test.go) <webdiff>/third_party/terminal-to-html/

Remove this copy, and the `replace` directive, when an upstream release
writes 24-bit colours.
