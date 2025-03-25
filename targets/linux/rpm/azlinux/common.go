package azlinux

import (
	"context"

	"github.com/Azure/dalec"
	"github.com/Azure/dalec/frontend"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
)

var (
	builderPackages = []string{
		"rpm-build",
		"mariner-rpm-macros",
		"build-essential",
		"ca-certificates",
	}

	targets = map[string]gwclient.BuildFunc{
		Mariner2TargetKey: Mariner2Config.Handle,
		AzLinux3TargetKey: Azlinux3Config.Handle,
	}

	defaultAzlinuxRepoPlatform = dalec.RepoPlatformConfig{
		ConfigRoot: "/etc/yum.repos.d",
		GPGKeyRoot: "/etc/pki/rpm-gpg",
		ConfigExt:  ".repo",
	}
)

func Handlers(ctx context.Context, client gwclient.Client, m *frontend.BuildMux) error {
	return frontend.LoadBuiltinTargets(targets)(ctx, client, m)
}

func basePackages(name string) []dalec.Spec {
	const (
		distMin  = "distroless-packages-minimal"
		prebuilt = "prebuilt-ca-certificates"
		base     = "dalec-base-"
		license  = "Apache-2.0"
		version  = "0.0.1"
		rev      = "1"
	)

	return []dalec.Spec{
		{
			Name:        base + name,
			Version:     version,
			Revision:    rev,
			License:     license,
			Description: "DALEC base packages for " + name,
			Dependencies: &dalec.PackageDependencies{
				Runtime: map[string]dalec.PackageConstraints{
					distMin: {},
				},
				Recommends: map[string]dalec.PackageConstraints{
					prebuilt: {},
				},
			},
		},
	}
}
