package main

import (
	"flag"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
	"go.coder.com/flog"
)

type globalFlags struct {
	verbose    bool
	configPath string
}

func (gf *globalFlags) debug(msg string, args ...interface{}) {
	if !gf.verbose {
		return
	}

	flog.Log(
		flog.Level(color.New(color.FgHiMagenta).Sprint("DEBUG")),
		msg, args...,
	)
}

func (gf *globalFlags) config() config {
	return mustReadConfig(gf.configPath)
}

// ensureDockerDaemon verifies that Docker is running.
func (gf *globalFlags) ensureDockerDaemon() {
	out, err := exec.Command("docker", "info").CombinedOutput()
	if err != nil {
		flog.Fatal("failed to run `docker info`: %v\n%s", err, out)
	}
	gf.debug("verified Docker is running")
}

func requireRepo(conf config, prefs schemaPrefs, fl *flag.FlagSet) repo {
	repoURI := fl.Arg(0)
	if repoURI == "" {
		flog.Fatal("Argument <repo> must be provided.")
	}

	if pathIsRunnable(conf, repoURI) {

	}

	r, err := parseRepo(defaultSchema(conf, prefs), repoURI)
	if err != nil {
		flog.Fatal("failed to parse repo %q: %v", repoURI, err)
	}
	return r
}

func pathIsRunnable(conf config, path string) bool {
	fp, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	s, err := os.Stat(fp)
	if err != nil {
		return false
	}

	if !s.IsDir() {
		return false
	}

	if match, _ := filepath.Match(expandRoot(conf.ProjectRoot), fp); !match {
		return false
	}

	split := strings.Split(fp, "/")
	if len(split) < 2 {
		return false
	}

	return true
}

func expandRoot(path string) string {
	u, _ := user.Current()
	return strings.Replace(path, "~/", u.HomeDir+"/", 1)
}

func defaultSchema(conf config, prefs schemaPrefs) string {
	switch {
	case prefs.ssh:
		return "ssh"
	case prefs.https:
		return "https"
	case prefs.http:
		return "http"
	case conf.DefaultSchema != "":
		return conf.DefaultSchema
	default:
		return "ssh"
	}
}

// project reads the project as the first parameter.
func (gf *globalFlags) project(prefs schemaPrefs, fl *flag.FlagSet) *project {
	conf := gf.config()
	return &project{
		conf: conf,
		repo: requireRepo(conf, prefs, fl),
	}
}
