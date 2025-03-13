package almalinux

import (
	"context"

	"github.com/Azure/dalec"
	"github.com/Azure/dalec/frontend"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
)

var (
	builderPackages = []string{
		"binutils",
		"rpm-build",
		"ca-certificates",
	}

	targets = map[string]gwclient.BuildFunc{
		v8TargetKey: ConfigV8.Handle,
		v9TargetKey: ConfigV9.Handle,
	}

	defaultPlatformConfig = dalec.RepoPlatformConfig{
		ConfigRoot: "/etc/yum.repos.d",
		GPGKeyRoot: "/etc/pki/rpm-gpg",
		ConfigExt:  ".repo",
	}
)

func Handlers(ctx context.Context, client gwclient.Client, m *frontend.BuildMux) error {
	return frontend.LoadBuiltinTargets(targets)(ctx, client, m)
}
