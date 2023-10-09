package mariner2

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/azure/dalec"
	"github.com/azure/dalec/frontend"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/image"
	"github.com/moby/buildkit/frontend/dockerui"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
)

const (
	marinerRef            = "mcr.microsoft.com/cbl-mariner/base/core:2.0"
	toolchainImgRef       = "ghcr.io/azure/dalec/mariner2/toolchain:latest"
	toolchainNamedContext = "mariner2-toolchain"

	cachedRpmsDir      = "/root/.cache/mariner2-toolkit-rpm-cache"
	cachedRpmsName     = "mariner2-toolkit-cached-rpms"
	marinerToolkitPath = "/usr/local/toolkit"
)

var (
	marinerTdnfCache = llb.AddMount("/var/tdnf/cache", llb.Scratch(), llb.AsPersistentCacheDir("mariner2-tdnf-cache", llb.CacheMountLocked))
)

var _ dockerUIClient = (*dockerui.Client)(nil)

type dockerUIClient interface {
	MainContext(ctx context.Context, opts ...llb.LocalOption) (*llb.State, error)
	NamedContext(ctx context.Context, name string, opts dockerui.ContextOpt) (*llb.State, *image.Image, error)
}

func handleRPM(ctx context.Context, client gwclient.Client, spec *dalec.Spec) (gwclient.Reference, *image.Image, error) {
	caps := client.BuildOpts().LLBCaps
	noMerge := !caps.Contains(pb.CapMergeOp)

	baseImg, err := getBaseBuilderImg(ctx, client)
	if err != nil {
		return nil, nil, err
	}

	st, err := specToRpmLLB(spec, noMerge, getDigestFromClientFn(ctx, client), client, frontend.ForwarderFromClient(ctx, client), baseImg)
	if err != nil {
		return nil, nil, err
	}

	def, err := st.Marshal(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("error marshalling llb: %w", err)
	}

	res, err := client.Solve(ctx, gwclient.SolveRequest{
		Definition: def.ToPB(),
	})
	if err != nil {
		return nil, nil, err
	}
	ref, err := res.SingleRef()
	// Do not return a nil image, it may cause a panic
	return ref, &image.Image{}, err
}

func shArgs(cmd string) llb.RunOption {
	return llb.Args([]string{"sh", "-c", cmd})
}

func getBuildDeps(spec *dalec.Spec) []string {
	var deps *dalec.PackageDependencies
	if t, ok := spec.Targets[targetKey]; ok {
		deps = t.Dependencies
	}

	if deps == nil {
		deps = spec.Dependencies
		if deps == nil {
			return nil
		}
	}

	var out []string
	for p := range deps.Build {
		out = append(out, p)
	}

	sort.Strings(out)
	return out
}

func getBaseBuilderImg(ctx context.Context, client gwclient.Client) (llb.State, error) {
	dc, err := dockerui.NewClient(client)
	if err != nil {
		return llb.Scratch(), err
	}

	// Check if the client passed in a named context for the toolkit.
	namedSt, cfg, err := dc.NamedContext(ctx, toolchainNamedContext, dockerui.ContextOpt{})
	if err != nil {
		return llb.Scratch(), err
	}

	if namedSt != nil {
		if cfg != nil {
			dt, err := json.Marshal(cfg)
			if err != nil {
				return llb.Scratch(), err
			}
			return namedSt.WithImageConfig(dt)
		}
		return *namedSt, nil
	}

	// See if there is a named context using the toolchain image ref
	namedSt, cfg, err = dc.NamedContext(ctx, toolchainImgRef, dockerui.ContextOpt{})
	if err != nil {
		return llb.Scratch(), err
	}

	if namedSt != nil {
		if cfg != nil {
			dt, err := json.Marshal(cfg)
			if err != nil {
				return llb.Scratch(), err
			}
			return namedSt.WithImageConfig(dt)
		}
		return *namedSt, nil
	}

	return llb.Image(marinerRef, llb.WithMetaResolver(client)), nil
}

func specToRpmLLB(spec *dalec.Spec, noMerge bool, getDigest getDigestFunc, mr llb.ImageMetaResolver, forward dalec.ForwarderFunc, baseImg llb.State) (llb.State, error) {
	br, err := spec2ToolkitRootLLB(spec, noMerge, getDigest, mr, forward)
	if err != nil {
		return llb.Scratch(), err
	}

	specsDir := "/build/SPECS"

	work := baseImg.
		// /.dockerenv is used by the toolkit to detect it's running in a container.
		// This makes the toolkit use a different strategy for setting up chroots.
		// Namely, it makes so the toolkit won't use "mount" to mount the stuff into the chroot which requires CAP_SYS_ADMIN.
		// (CAP_SYS_ADMIN is not enabled in our build).
		File(llb.Mkfile("/.dockerenv", 0o600, []byte{})).
		Dir("/build/toolkit").
		AddEnv("SPECS_DIR", specsDir).
		AddEnv("CONFIG_FILE", ""). // This is needed for VM images(?), don't need this for our case anyway and the default value is wrong for us.
		AddEnv("OUT_DIR", "/build/out").
		AddEnv("LOG_LEVEL", "debug").
		AddEnv("CACHED_RPMS_DIR", cachedRpmsDir)

	prepareChroot := runOptFunc(func(ei *llb.ExecInfo) {
		// Mount cached packages into the chroot dirs so they are available to the chrooted build.
		// The toolchain has built-in (yum) repo files that points to "/upstream-cached-rpms",
		// so "tdnf install" as performed by the toolchain will read files from this location in the chroot.
		//
		// The toolchain can also run things in parallel so it will use multiple chroots.
		// See https://github.com/microsoft/CBL-Mariner/blob/8b1db59e9b011798e8e7750907f58b1bc9577da7/toolkit/tools/internal/buildpipeline/buildpipeline.go#L37-L117 for implementation of this.
		//
		// This is needed because the toolkit cannot mount anything into the chroot as it doesn't have CAP_SYS_ADMIN in our build.
		// So we have to mount the cached packages into the chroot dirs ourselves.
		dir := func(i int, p string) string {
			return filepath.Join("/tmp/chroot", "dalec"+strconv.Itoa(i), p)
		}
		for i := 0; i < runtime.NumCPU(); i++ {
			llb.AddMount(dir(i, "upstream-cached-rpms"), llb.Scratch(), llb.AsPersistentCacheDir(cachedRpmsName, llb.CacheMountLocked)).SetRunOption(ei)
		}
	})

	specsMount := llb.AddMount(specsDir, br, llb.SourcePath("/SPECS"))
	cachedRPMsMount := llb.AddMount(filepath.Join(cachedRpmsDir, "cache"), llb.Scratch(), llb.AsPersistentCacheDir(cachedRpmsName, llb.CacheMountLocked))

	st := work.With(func(st llb.State) llb.State {
		deps := getBuildDeps(spec)
		if len(deps) == 0 {
			return st
		}
		return st.Run(
			shArgs("dnf download -y --releasever=2.0 --resolve --alldeps --downloaddir \"${CACHED_RPMS_DIR}/cache\" "+strings.Join(getBuildDeps(spec), " ")),
			cachedRPMsMount,
		).State
	}).
		Run(
			shArgs("make -j$(nproc) build-packages || (cat /build/build/logs/pkggen/rpmbuilding/*; exit 1)"),
			prepareChroot,
			cachedRPMsMount,
			specsMount,
			marinerTdnfCache,
			llb.AddEnv("VERSION", spec.Version),
			llb.AddEnv("BUILD_NUMBER", spec.Revision),
			llb.AddEnv("REFRESH_WORKER_CHROOT", "n"),
		).State

	return llb.Scratch().File(
		llb.Copy(st, "/build/out", "/", dalec.WithDirContentsOnly(), dalec.WithIncludes([]string{"RPMS", "SRPMS"})),
	), nil
}

type runOptFunc func(*llb.ExecInfo)

func (f runOptFunc) SetRunOption(ei *llb.ExecInfo) {
	f(ei)
}
