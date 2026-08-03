// Command image-volume-converter implements the "no-arch image converter"
// GitHub Action. It loads an image (preferring the local docker daemon,
// otherwise pulling it for the platform of the runner), exports it as an OCI
// archive and unpacks it into an OCI layout directory, rewrites the image
// config so that `architecture` and `os` are `unknown`, and finally either
// pushes the resulting image to a remote registry or writes it as an OCI
// archive to a local file.
package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "image-volume-converter:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	imageTag := os.Getenv("INPUT_IMAGE_TAG")
	outputImageTag := os.Getenv("INPUT_OUTPUT_IMAGE_TAG")
	publishTo := os.Getenv("INPUT_PUBLISH_TO")
	if publishTo == "" {
		publishTo = "RemotePush"
	}
	username := os.Getenv("INPUT_REGISTRY_USERNAME")
	password := os.Getenv("INPUT_REGISTRY_PASSWORD")
	actor := os.Getenv("INPUT_GITHUB_ACTOR")
	token := os.Getenv("INPUT_GITHUB_TOKEN")

	if imageTag == "" {
		return errors.New("INPUT_IMAGE_TAG is required")
	}
	if username == "" {
		username = actor
	}
	if password == "" {
		password = token
	}

	dst, err := resolveDestination(imageTag, outputImageTag, publishTo)
	if err != nil {
		return err
	}
	if dst.mode == "RemotePush" && password == "" {
		fmt.Fprintln(os.Stderr, "image-volume-converter: warning: no registry credentials provided; pushing to a remote registry may fail unless the repository is public")
	}

	srcRef, err := name.ParseReference(imageTag)
	if err != nil {
		return fmt.Errorf("parsing imageTag %q: %w", imageTag, err)
	}

	auth := authn.Anonymous
	if password != "" {
		auth = authn.FromConfig(authn.AuthConfig{Username: username, Password: password})
	}

	platform, err := runnerPlatform()
	if err != nil {
		return err
	}

	// 1) Load the image: prefer the local docker daemon, otherwise pull it for
	//    the platform of the current runner.
	img, source, err := loadImage(ctx, srcRef, platform, auth)
	if err != nil {
		return err
	}
	fmt.Printf("image-volume-converter: resolved %s from %s\n", srcRef, source)

	// 1b) Export the image as an OCI archive and unpack it into an OCI layout
	//     directory. This mirrors:
	//       podman save --format oci-archive -o /tmp/docs.tar <image>
	//       tar -xf /tmp/docs.tar -C /tmp/docs-oci
	workDir := filepath.Join(os.TempDir(), "image-volume-converter")
	archivePath := filepath.Join(workDir, "docs.tar")
	ociDir := filepath.Join(workDir, "docs-oci")
	if err := exportOCILayout(workDir, archivePath, ociDir, img); err != nil {
		return err
	}

	// 2) Resolve index.json -> manifest -> config, reading the blobs exactly as
	//    if the unpacked directory had been inspected by hand.
	lp, err := layout.FromPath(ociDir)
	if err != nil {
		return fmt.Errorf("reading OCI layout %s: %w", ociDir, err)
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		return fmt.Errorf("reading OCI index: %w", err)
	}
	idx, err := ii.IndexManifest()
	if err != nil {
		return fmt.Errorf("reading index.json: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return errors.New("index.json contains no manifests")
	}
	manifestDesc := idx.Manifests[0]
	manifestImg, err := ii.Image(manifestDesc.Digest)
	if err != nil {
		return fmt.Errorf("reading manifest %s: %w", manifestDesc.Digest, err)
	}
	configName, err := manifestImg.ConfigName()
	if err != nil {
		return err
	}
	cfg, err := manifestImg.ConfigFile()
	if err != nil {
		return fmt.Errorf("reading config %s: %w", configName, err)
	}
	fmt.Printf("image-volume-converter: index.json -> manifest %s -> config %s (architecture=%q os=%q)\n",
		manifestDesc.Digest, configName, cfg.Architecture, cfg.OS)

	// 3) Rewrite the config: architecture and os both become "unknown", then
	//    repack into a fresh OCI layout (new config blob, manifest and index).
	fmt.Printf("image-volume-converter: setting architecture and os to \"unknown\"\n")
	cfg.Architecture = "unknown"
	cfg.OS = "unknown"
	noArch, err := mutate.ConfigFile(manifestImg, cfg)
	if err != nil {
		return fmt.Errorf("updating config: %w", err)
	}

	ociNewDir := filepath.Join(workDir, "docs-image-volume-converterd")
	if err := writeLayout(ociNewDir, noArch); err != nil {
		return err
	}
	outImg, err := imageFromLayout(ociNewDir)
	if err != nil {
		return fmt.Errorf("reading no-arch layout: %w", err)
	}
	outCfg, err := outImg.ConfigFile()
	if err != nil {
		return err
	}
	if outCfg.Architecture != "unknown" || outCfg.OS != "unknown" {
		return errors.New("os/arch were not set to unknown")
	}
	newConfigName, err := outImg.ConfigName()
	if err != nil {
		return err
	}
	fmt.Printf("image-volume-converter: no-arch layout written to %s (config %s, architecture=%q os=%q)\n",
		ociNewDir, newConfigName, outCfg.Architecture, outCfg.OS)

	// 4) Output the no-arch image: push it to a remote registry, or export it
	//    as an OCI archive to a local file.
	switch dst.mode {
	case "OCIArchive":
		fmt.Printf("image-volume-converter: exporting %s as OCI archive to %s\n", srcRef, dst.path)
		if err := exportOCIArchive(dst.path, ociNewDir); err != nil {
			return err
		}
		fmt.Printf("image-volume-converter: exported OCI archive to %s\n", dst.path)
	case "RemotePush":
		fmt.Printf("image-volume-converter: publishing %s -> %s\n", srcRef, dst.ref)
		if err := remote.Write(dst.ref, outImg,
			remote.WithAuth(auth),
			remote.WithContext(ctx),
		); err != nil {
			return fmt.Errorf("publishing image: %w", err)
		}
		fmt.Printf("image-volume-converter: published %s\n", dst.ref.Name())
	default:
		return fmt.Errorf("internal error: unknown destination mode %q", dst.mode)
	}
	return nil
}

// destination describes where the no-arch image is written.
type destination struct {
	// mode is "RemotePush" or "OCIArchive".
	mode string
	// ref is the remote reference to push to when mode is RemotePush.
	ref name.Reference
	// path is the output archive file when mode is OCIArchive.
	path string
}

// resolveDestination validates the publishTo mode and returns where the
// converted image should be written.
//
// With publishTo: RemotePush the destination is the remote reference
// outputImageTag. When outputImageTag is empty it is defaulted to the input
// imageTag with "-noarch" appended to its tag (see defaultRemoteTarget).
//
// With publishTo: OCIArchive:<path> the destination is a local OCI archive
// written to <path>; outputImageTag is ignored.
func resolveDestination(imageTag, outputImageTag, publishTo string) (destination, error) {
	mode, arg, hasArg := strings.Cut(publishTo, ":")
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "remotepush":
		if outputImageTag == "" {
			outputImageTag = defaultRemoteTarget(imageTag)
		}
		ref, err := name.ParseReference(outputImageTag)
		if err != nil {
			return destination{}, fmt.Errorf("parsing outputImageTag %q: %w", outputImageTag, err)
		}
		return destination{mode: "RemotePush", ref: ref}, nil
	case "ociarchive":
		if !hasArg || strings.TrimSpace(arg) == "" {
			return destination{}, errors.New("publishTo OCIArchive requires an output file path, e.g. OCIArchive:/tmp/image.tar")
		}
		return destination{mode: "OCIArchive", path: strings.TrimSpace(arg)}, nil
	default:
		return destination{}, fmt.Errorf("invalid publishTo %q (supported: RemotePush, OCIArchive:<path>)", publishTo)
	}
}

// defaultRemoteTarget returns a remote destination for a missing
// outputImageTag: the input imageTag with "-noarch" appended to its tag, so the
// converted image is written to a distinct reference instead of overwriting the
// source. For example ghcr.io/kubehub-io/docs:main becomes
// ghcr.io/kubehub-io/docs:main-noarch. An input without a tag is treated as
// :latest, so it defaults to <repository>:latest-noarch.
func defaultRemoteTarget(imageTag string) string {
	ref, err := name.ParseReference(imageTag, name.WithDefaultTag("latest"))
	if err != nil {
		return imageTag + ":no-arch"
	}
	if _, ok := ref.(name.Tag); ok {
		return strings.TrimSuffix(imageTag, ":"+ref.Identifier()) + ":" + ref.Identifier() + "-noarch"
	}
	// Digest references cannot carry a tag; fall back to the same repository.
	return ref.Context().Name() + ":no-arch"
}

// loadImage returns the image from the local docker daemon when available,
// falling back to pulling it from its registry for the given platform.
func loadImage(ctx context.Context, ref name.Reference, platform v1.Platform, auth authn.Authenticator) (v1.Image, string, error) {
	if img, err := daemon.Image(ref); err == nil {
		return img, "local docker daemon", nil
	}
	fmt.Printf("image-volume-converter: image not found in local docker daemon, pulling %s (platform %s/%s)\n",
		ref, platform.OS, platform.Architecture)
	img, err := remote.Image(ref,
		remote.WithPlatform(platform),
		remote.WithAuth(auth),
		remote.WithContext(ctx),
	)
	if err != nil {
		return nil, "", fmt.Errorf("pulling image %s: %w", ref, err)
	}
	return img, "remote registry", nil
}

// exportOCILayout writes the image into an OCI layout directory, packs it into
// an OCI archive and then unpacks the archive into the final directory.
func exportOCILayout(workDir, archivePath, ociDir string, img v1.Image) error {
	if err := os.RemoveAll(workDir); err != nil {
		return err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	staging := filepath.Join(workDir, "staging")
	if err := writeLayout(staging, img); err != nil {
		return err
	}
	if err := packDir(staging, archivePath); err != nil {
		return err
	}
	if err := unpack(archivePath, ociDir); err != nil {
		return err
	}
	fmt.Printf("image-volume-converter: exported OCI archive to %s and unpacked it to %s\n", archivePath, ociDir)
	return nil
}

// writeLayout creates a fresh OCI layout directory containing the single image.
func writeLayout(dir string, img v1.Image) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	path, err := layout.Write(dir, empty.Index)
	if err != nil {
		return err
	}
	return path.AppendImage(img)
}

// imageFromLayout resolves the single image described by an OCI layout
// directory's index.json (index.json -> manifest -> image).
func imageFromLayout(dir string) (v1.Image, error) {
	lp, err := layout.FromPath(dir)
	if err != nil {
		return nil, err
	}
	ii, err := lp.ImageIndex()
	if err != nil {
		return nil, err
	}
	idx, err := ii.IndexManifest()
	if err != nil {
		return nil, err
	}
	if len(idx.Manifests) == 0 {
		return nil, errors.New("layout contains no manifests")
	}
	return ii.Image(idx.Manifests[0].Digest)
}

// exportOCIArchive packs the OCI layout directory ociDir into a tar archive at
// path (creating parent directories as needed).
func exportOCIArchive(path, ociDir string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return packDir(ociDir, path)
}

// packDir creates a tar archive of srcDir with paths relative to srcDir (the
// on-disk representation of an OCI archive).
func packDir(srcDir, archivePath string) error {
	f, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, src)
		src.Close()
		return copyErr
	})
}

// unpack extracts a tar archive into destDir, guarding against path traversal.
func resolvedWithin(baseAbs, candidate string) (string, bool, error) {
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", false, err
	}

	parentResolved, err := filepath.EvalSymlinks(filepath.Dir(candidateAbs))
	if err != nil {
		return "", false, err
	}

	resolved := filepath.Join(parentResolved, filepath.Base(candidateAbs))
	rel, err := filepath.Rel(baseAbs, resolved)
	if err != nil {
		return "", false, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return resolved, false, nil
	}
	return resolved, true, nil
}

func unpack(archivePath, destDir string) error {
	if err := os.RemoveAll(destDir); err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	destAbs, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || name == "" || filepath.IsAbs(name) {
			continue
		}
		if name == ".." ||
			strings.HasPrefix(name, ".."+string(os.PathSeparator)) ||
			strings.Contains(name, string(os.PathSeparator)+".."+string(os.PathSeparator)) ||
			strings.HasSuffix(name, string(os.PathSeparator)+"..") {
			continue
		}

		target := filepath.Join(destDir, name)
		targetAbs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(destAbs, targetAbs)
		if err != nil {
			return err
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if _, ok, err := resolvedWithin(destAbs, target); err != nil {
				return err
			} else if !ok {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			out.Close()
			if copyErr != nil {
				return copyErr
			}
		case tar.TypeSymlink:
			if _, ok, err := resolvedWithin(destAbs, target); err != nil {
				return err
			} else if !ok {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			linkCandidate := filepath.Join(filepath.Dir(target), hdr.Linkname)
			if _, ok, err := resolvedWithin(destAbs, linkCandidate); err != nil {
				return err
			} else if !ok {
				continue
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
	return nil
}

// runnerPlatform maps the architecture of the current runner to the OCI
// platform used when pulling a multi-arch image.
func runnerPlatform() (v1.Platform, error) {
	switch runtime.GOARCH {
	case "amd64":
		return v1.Platform{OS: "linux", Architecture: "amd64"}, nil
	case "arm64":
		return v1.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, nil
	case "arm":
		return v1.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}, nil
	case "386":
		return v1.Platform{OS: "linux", Architecture: "386"}, nil
	default:
		return v1.Platform{}, fmt.Errorf("unsupported runner architecture %q", runtime.GOARCH)
	}
}
