package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/contrib/apparmor"
	"github.com/containerd/containerd/contrib/nvidia"
	"github.com/containerd/containerd/contrib/seccomp"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/typeurl"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	Root      = "/var/lib/boss"
	Extension = "io.boss/container"
)

func init() {
	typeurl.Register(&Container{}, "io.boss.v1.Container")
}

type NetworkType string

const (
	Host NetworkType = "host"
	CNI  NetworkType = "cni"
	None NetworkType = ""
)

type Container struct {
	ID        string             `toml:"id"`
	Image     string             `toml:"image"`
	Resources *Resources         `toml:"resources"`
	GPUs      *GPUs              `toml:"gpus"`
	Mounts    []Mount            `toml:"mounts"`
	Env       []string           `toml:"env"`
	Args      []string           `toml:"args"`
	Labels    []string           `toml:"labels"`
	Network   NetworkType        `toml:"network"`
	Services  map[string]Service `toml:"services"`
}

// WithBossConfig is a containerd.NewContainerOpts for spec and container configuration
func WithBossConfig(config *Container, image containerd.Image) containerd.NewContainerOpts {
	return func(ctx context.Context, client *containerd.Client, c *containers.Container) error {
		// generate the spec
		if err := containerd.WithNewSpec(config.specOpt(image))(ctx, client, c); err != nil {
			return err
		}
		// set boss labels
		if err := containerd.WithContainerLabels(toStrings(config.Labels))(ctx, client, c); err != nil {
			return err
		}
		// save the config as a container extension
		return containerd.WithContainerExtension(Extension, config)(ctx, client, c)
	}
}

func (config *Container) specOpt(image containerd.Image) oci.SpecOpts {
	opts := []oci.SpecOpts{
		oci.WithImageConfigArgs(image, config.Args),
		oci.WithHostLocaltime,
		oci.WithNoNewPrivileges,
		apparmor.WithDefaultProfile("boss"),
		seccomp.WithDefaultProfile(),
		oci.WithEnv(config.Env),
		withMounts(config.Mounts),
	}
	if config.Network == Host {
		opts = append(opts, oci.WithHostHostsFile, oci.WithHostResolvconf, oci.WithHostNamespace(specs.NetworkNamespace))
	} else {
		opts = append(opts, withBossResolvconf, withContainerHostsFile)
	}
	if config.Resources != nil {
		opts = append(opts, withResources(config.Resources))
	}
	if config.GPUs != nil {
		opts = append(opts, nvidia.WithGPUs(
			nvidia.WithDevices(config.GPUs.Devices...),
			nvidia.WithCapabilities(toGpuCaps(config.GPUs.Capbilities)...),
		),
		)
	}
	return oci.Compose(opts...)
}

type Service struct {
	Port   int      `toml:"port"`
	Labels []string `toml:"labels"`
	Checks []Check  `toml:"checks"`
}

type CheckType string

const (
	HTTP CheckType = "http"
	TCP  CheckType = "tcp"
	GRPC CheckType = "grpc"
)

type Check struct {
	Type     CheckType `toml:"type"`
	Interval int       `toml:"interval"`
	Timeout  int       `toml:"timeout"`
}

type Resources struct {
	CPU    float64 `toml:"cpu"`
	Memory int64   `toml:"memory"`
	Score  int     `toml:"score"`
}

type GPUs struct {
	Devices     []int    `toml:"devices"`
	Capbilities []string `toml:"capabilities"`
}

type Mount struct {
	Type        string   `toml:"type"`
	Source      string   `toml:"source"`
	Destination string   `toml:"destination"`
	Options     []string `toml:"options"`
}

func toStrings(ss []string) map[string]string {
	m := make(map[string]string, len(ss))
	for _, s := range ss {
		parts := strings.SplitN(s, "=", 2)
		m[parts[0]] = parts[1]
	}
	return m
}

func toGpuCaps(ss []string) (o []nvidia.Capability) {
	for _, s := range ss {
		o = append(o, nvidia.Capability(s))
	}
	return o
}

func withResources(r *Resources) oci.SpecOpts {
	return func(ctx context.Context, _ oci.Client, c *containers.Container, s *oci.Spec) error {
		if r.Memory > 0 {
			limit := r.Memory * 1024 * 1024
			s.Linux.Resources.Memory = &specs.LinuxMemory{
				Limit: &limit,
			}
		}
		if r.CPU > 0 {
			period := uint64(100000)
			quota := int64(r.CPU * 100000.0)
			s.Linux.Resources.CPU = &specs.LinuxCPU{
				Quota:  &quota,
				Period: &period,
			}
		}
		if r.Score != 0 {
			s.Process.OOMScoreAdj = &r.Score
		}
		return nil
	}
}

func withMounts(mounts []Mount) oci.SpecOpts {
	return func(ctx context.Context, _ oci.Client, c *containers.Container, s *oci.Spec) error {
		for _, cm := range mounts {
			if cm.Type == "bind" {
				// create source if it does not exist
				if err := os.MkdirAll(filepath.Dir(cm.Source), 0755); err != nil {
					return err
				}
				if err := os.Mkdir(cm.Source, 0755); err != nil {
					if !os.IsExist(err) {
						return err
					}
				} else {
					if err := os.Chown(cm.Source, int(s.Process.User.UID), int(s.Process.User.GID)); err != nil {
						return err
					}
				}
			}
			s.Mounts = append(s.Mounts, specs.Mount{
				Type:        cm.Type,
				Source:      cm.Source,
				Destination: cm.Destination,
				Options:     cm.Options,
			})
		}
		return nil
	}
}

func withContainerHostsFile(ctx context.Context, _ oci.Client, c *containers.Container, s *oci.Spec) error {
	id := c.ID
	if err := os.MkdirAll(filepath.Join(Root, id), 0711); err != nil {
		return err
	}
	hostname := s.Hostname
	if hostname == "" {
		hostname = id
	}
	path := filepath.Join(Root, id, "hosts")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("127.0.0.1       localhost\n"); err != nil {
		return err
	}
	if _, err := f.WriteString(fmt.Sprintf("127.0.0.1       %s\n", hostname)); err != nil {
		return err
	}
	if _, err := f.WriteString("::1     localhost ip6-localhost ip6-loopback\n"); err != nil {
		return err
	}
	s.Mounts = append(s.Mounts, specs.Mount{
		Destination: "/etc/hosts",
		Type:        "bind",
		Source:      path,
		Options:     []string{"rbind", "ro"},
	})
	return nil
}

func withBossResolvconf(ctx context.Context, _ oci.Client, c *containers.Container, s *oci.Spec) error {
	s.Mounts = append(s.Mounts, specs.Mount{
		Destination: "/etc/resolv.conf",
		Type:        "bind",
		Source:      filepath.Join(Root, "resolv.conf"),
		Options:     []string{"rbind", "ro"},
	})
	return nil
}
