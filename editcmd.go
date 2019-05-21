package main

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types"

	"go.coder.com/cli"
	"go.coder.com/flog"
	"go.coder.com/sail/internal/dockutil"
	"go.coder.com/sail/internal/editor"
	"go.coder.com/sail/internal/randstr"
	"go.coder.com/sail/internal/xexec"
	"golang.org/x/xerrors"
)

type editcmd struct {
	gf *globalFlags

	noEditor bool
	hatPath  string
	hat      bool
}

func (c *editcmd) Spec() cli.CommandSpec {
	return cli.CommandSpec{
		Name:  "edit",
		Usage: "[flags] <repo>",
		Desc: `This command allows you to edit your project's environment while it's running.
Depending on what flags are set, the Dockerfile you want to change will be opened in your default
editor which can be set using the "EDITOR" environment variable. Once your changes are complete
and the editor is closed, the environment will be rebuilt and rerun with minimal downtime.

If no flags are set, this will open your project's Dockerfile. If the -hat flag is set, this
will open the hat Dockerfile associated with your running project in the editor. If the -new-hat
flag is set, the project will be adjusted to use the new hat.

VS Code users can edit their environment by editing their .sail/Dockerfile within the editor. VS Code
will rebuild the container on save.`,
	}
}

func (c *editcmd) Run(fl *flag.FlagSet) {
	proj := c.gf.project(schemaPrefs{}, fl)

	c.gf.ensureDockerDaemon()

	err := os.MkdirAll(filepath.Dir(proj.dockerfilePath()), 0755)
	if err != nil {
		flog.Fatal("failed to create intermediate dirs: %v", err)
	}

	// Create file if it doesn't already exist.
	fi, err := os.OpenFile(proj.dockerfilePath(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil && !os.IsExist(err) {
		flog.Fatal("failed to open %v: %v", proj.dockerfilePath())
	} else if err == nil {
		defer fi.Close()
		// Provide a sensible default Dockerfile if the image hasn't been customized.
		_, err = fi.WriteString("FROM codercom/ubuntu-dev\n")
		if err != nil {
			flog.Fatal("failed to write default Dockerfile: %v", err)
		}
		err = fi.Close()
		if err != nil {
			flog.Fatal("failed to write default Dockerfile")
		}
	}

	err = c.recreate(proj)
	if err != nil {
		flog.Fatal("%v", err)
	}
	os.Exit(0)
}

func (c *editcmd) recreate(proj *project) (err error) {
	cli := dockerClient()
	defer cli.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Get the existing container's state so re-create is seamless.
	b, err := hatBuilderFromContainer(proj.cntName())
	if err != nil {
		return err
	}

	editFile := proj.dockerfilePath()
	// If custom hat provided, use it.
	if c.hatPath != "" {
		b.hatPath = c.hatPath
	}

	// If c.hat is set, then we want to edit the project's hat instead of the project's Dockerfile.
	if c.hat {
		if b.hatPath == "" {
			return xerrors.New("unable to edit a nonexistent hat")
		}
		hatPath, err := b.resolveHatPath()
		if err != nil {
			return err
		}
		editFile = filepath.Join(hatPath, "Dockerfile")
	}

	// If we're just trying to change the underlying hat for the project, we don't want
	// to prompt the user with the editor, instead just rebuild with the new hat.
	if c.hatPath == "" || c.hat {
		err = runEditor(editFile)
		if err != nil {
			return err
		}
	}

	r, err := runnerFromContainer(proj.cntName())
	if err != nil {
		return xerrors.Errorf("failed to initialize runner: %w", err)
	}

	builderCntName := proj.cntName() + "-builder-" + randstr.Make(5)
	r.cntName = builderCntName

	image, ok, err := proj.buildImage()
	if err != nil {
		return xerrors.Errorf("failed to build image: %w", err)
	}
	// If we were previously using the default image, we must explicitely override
	// to use the new base.
	if ok {
		b.baseImage = image
	}

	// Apply the hat before we stop the original container in order to reduce the amount
	// of downtime and to prevent any downtime in the event of a failed hat application.
	if b.hatPath != "" {
		image, err = b.applyHat()
		if err != nil {
			return xerrors.Errorf("failed to apply hat: %w", err)
		}
	}

	// The base and hat images have been fully built, stop the original container to swap
	// it with the new one.
	err = cli.ContainerStop(ctx, proj.cntName(), dockutil.DurationPtr(time.Second))
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			flog.Error("failed to build and run new container: %v", err)
			flog.Info("rolling back...")

			err := cli.ContainerStart(ctx, proj.cntName(), types.ContainerStartOptions{})
			if err != nil {
				flog.Fatal("failed to restart original container %v in rollback: %v", proj.cntName(), err)
			}
		}
	}()

	// Rename OG container with a temporary name that we'll remove at the end if
	// everything completes successfully.
	oldCntName := proj.cntName() + "-old-" + randstr.Make(5)
	err = cli.ContainerRename(ctx, proj.cntName(), oldCntName)
	if err != nil {
		return xerrors.Errorf("failed to rename original container to %v: %w", oldCntName, err)
	}
	defer func() {
		// Roll the container rename back if something failed, but remove the old container from
		// the system if everything succeeded.
		if err != nil {
			err := cli.ContainerRename(ctx, oldCntName, proj.cntName())
			if err != nil {
				flog.Fatal("failed to rename container from %v back to %v in rollback: %v", oldCntName, proj.cntName(), err)
			}
		} else {
			_ = dockutil.StopRemove(ctx, cli, oldCntName)
		}
	}()

	// Start our new container and try to rename it to the project container name.
	err = r.runContainer(image)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err := dockutil.StopRemove(ctx, cli, builderCntName)
			if err != nil {
				flog.Error("failed to stop remove builder container in rollback: %v", err)
			}
		}
	}()

	err = cli.ContainerRename(ctx, r.cntName, proj.cntName())
	if err != nil {
		return xerrors.Errorf("failed to rename builder to project name: %w", err)
	}

	flog.Info("replaced container")
	return nil
}

func runEditor(file string) error {
	editor, err := editor.Env()
	if err != nil {
		return xerrors.Errorf("failed to get editor: %w", err)
	}
	// TODO: in an ideal world we could re-build the environment on each save instead of when the environment
	// quits. The problem is user feedback. For real-time edits, we must either send build feedback to the
	// calling terminal or start the editor in a completely different terminal. In the former option,
	// build feedback corrupts a terminal editor's layout. In the latter option, compatibility becomes
	// difficult, and sail will have a hard time being ran on server.

	cmd := exec.Command(editor, file)
	xexec.Attach(cmd)

	err = cmd.Run()
	if err != nil {
		return xerrors.Errorf("editor failed: %w", err)
	}

	return nil
}

var _ cli.FlaggedCommand = new(editcmd)

func (c *editcmd) RegisterFlags(fl *flag.FlagSet) {
	fl.StringVar(&c.hatPath, "new-hat", "", "Path to new hat.")
	fl.BoolVar(&c.hat, "hat", false, "Edit the hat associated with this project.")
}
