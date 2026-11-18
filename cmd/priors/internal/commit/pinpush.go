package commit

import (
	"context"
	"fmt"
	"strings"

	"github.com/noamsto/hookyard/cmd/priors/internal/pin"
	"github.com/noamsto/hookyard/cmd/priors/internal/tools"
)

// preflight refuses a push from the checkout at dir unless the trust file
// pins remote for it, the push environment is pinned, and dir matches its pin.
func preflight(ctx context.Context, dir, remote string) (warning string) {
	if remote == "" {
		return refusal(dir, "the trust file pins no remote for this store")
	}
	if _, err := pin.PushEnv(); err != nil {
		return refusal(dir, "%v", err)
	}
	// Re-checked under the store's lock, right before pushing.
	if err := pin.Check(ctx, dir, remote); err != nil {
		return refusal(dir, "%v", err)
	}
	return ""
}

// pinnedPush pushes src, a commit in the checkout at dir, to dstRef on remote
// with a lease that dstRef holds expect there, or does not exist when expect is
// "". Callers run preflight first. It pushes to the pinned URL from the
// build-pinned empty bare git dir, read-only and so beyond the owner's reach,
// reading the checkout's objects through GIT_OBJECT_DIRECTORY, in
// pin.PushEnv's environment: neither the checkout's config, where a
// [remote "<url>"] section redirects even an explicit-URL push, nor the
// caller's environment reaches it.
func pinnedPush(ctx context.Context, dir, remote, src, dstRef, expect string) (warning string) {
	format, warning := inspect(ctx, dir, "git rev-parse", "rev-parse", "--show-object-format")
	if warning != "" {
		return warning
	}
	if format != "sha1" {
		return refusal(dir, "the push repo is sha1; this checkout is %s", format)
	}
	common, warning := inspect(ctx, dir, "git rev-parse", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if warning != "" {
		return warning
	}
	beforePush()
	env, warning := pinnedEnv(ctx, dir, remote, common)
	if warning != "" {
		return warning
	}
	if _, err := runGitExact(ctx, tools.PushGitDir, env, pushTimeout, nil, "push", "-q", "--no-follow-tags", "--recurse-submodules=no", "--no-signed", "--force-with-lease="+dstRef+":"+expect, "--", remote, src+":"+dstRef); err != nil {
		return refusal(dir, "git push failed: %v", err)
	}
	return ""
}

// remoteHas reports whether ref exists on remote, asked from the pinned push
// git dir as pinnedPush pushes. Callers run preflight first.
func remoteHas(ctx context.Context, dir, remote, ref string) (exists bool, warning string) {
	common, warning := inspect(ctx, dir, "git rev-parse", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if warning != "" {
		return false, warning
	}
	env, warning := pinnedEnv(ctx, dir, remote, common)
	if warning != "" {
		return false, warning
	}
	out, err := runGitExact(ctx, tools.PushGitDir, env, pushTimeout, nil, "ls-remote", "--", remote, ref)
	if err != nil {
		return false, refusal(dir, "%s", gitWarning("git ls-remote", out, err))
	}
	// ls-remote matches a pattern against any ref's tail.
	for line := range strings.Lines(out) {
		if _, name, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "\t"); ok && name == ref {
			return true, ""
		}
	}
	return false, ""
}

// pinnedEnv is the environment for git in tools.PushGitDir reading the
// objects under common, the checkout's absolute git common dir, once remote's
// effective URL there is remote itself.
func pinnedEnv(ctx context.Context, dir, remote, common string) (env []string, warning string) {
	env, err := pin.ObjectsEnv(common)
	if err != nil {
		return nil, refusal(dir, "%v", err)
	}
	url, err := runGitExact(ctx, tools.PushGitDir, env, gitTimeout, nil, "ls-remote", "--get-url", remote)
	if err != nil {
		return nil, refusal(dir, "%s", gitWarning("git ls-remote", url, err))
	}
	if url = strings.TrimSpace(url); url != remote {
		return nil, refusal(dir, "effective push URL %s differs from the pinned %s", url, remote)
	}
	return env, ""
}

// refusal is a push refusal for the checkout at dir.
func refusal(dir, format string, args ...any) string {
	return fmt.Sprintf("not pushed from %s: ", dir) + fmt.Sprintf(format, args...)
}

// inspect runs a read-only git query in the checkout at dir, returning its
// trimmed output or a refusal quoting git's complaint.
func inspect(ctx context.Context, dir, what string, args ...string) (out, warning string) {
	out, err := runGit(ctx, dir, gitTimeout, nil, args...)
	if err != nil {
		return "", refusal(dir, "%s", gitWarning(what, out, err))
	}
	return strings.TrimSpace(out), ""
}
