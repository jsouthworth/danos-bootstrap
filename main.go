package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"

	"github.com/danos/utils/tsort"
	git "github.com/go-git/go-git/v5"
	"github.com/google/go-github/github"
	bpkg "jsouthworth.net/go/danos-buildpackage"
	"pault.ag/go/debian/control"
)

var (
	clone   bool
	build   bool
	srcDir  string
	pkgDir  string
	logDir  string
	version string
)

func resolvePath(in string) string {
	out, err := filepath.Abs(in)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return out
}

type buildError struct {
	repo string
	err  error
}

func (e buildError) Error() string {
	return fmt.Sprintf("build for %s failed: %s", e.repo, e.err)
}

type cloneError struct {
	repo string
	err  error
}

func (e cloneError) Error() string {
	return fmt.Sprintf("clone for %s failed: %s", e.repo, e.err)
}

type errList []error

func (l errList) Error() string {
	var buf bytes.Buffer
	for _, err := range l {
		fmt.Fprintln(&buf, err)
	}
	return buf.String()
}

func cloneRepos(into string) error {
	client := github.NewClient(nil)

	opt := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	// get all pages of results
	var allRepos []*github.Repository
	ctx := context.Background()
	for {
		repos, resp, err := client.Repositories.ListByOrg(ctx,
			"danos", opt)
		if err != nil {
			return err
		}
		allRepos = append(allRepos, repos...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}

	var cloneErrs errList
	for _, repo := range allRepos {
		if repo.Archived != nil && *repo.Archived {
			continue
		}
		fmt.Println("Cloning", *repo.Name, "from", *repo.CloneURL)
		_, err := git.PlainClone(filepath.Join(into, *repo.Name), false,
			&git.CloneOptions{
				URL:      *repo.CloneURL,
				Progress: os.Stdout,
			})
		if err != nil {
			cloneErrs = append(cloneErrs,
				cloneError{repo: *repo.Name, err: err})
		}
	}
	if len(cloneErrs) != 0 {
		return cloneErrs
	}
	return nil
}

type repoMetaData struct {
	ctrlFiles   map[string]*control.Control
	pack2repo   map[string]string
	unparseable []string
}

func enumerateBuildableRepos(from string) repoMetaData {
	out := repoMetaData{
		unparseable: []string{},
		ctrlFiles:   make(map[string]*control.Control),
		pack2repo:   make(map[string]string),
	}
	repos, err := ioutil.ReadDir(from)
	if err != nil {
		panic(err)
	}
	for _, repo := range repos {
		path := filepath.Join(from, repo.Name(), "debian", "control")
		ctrlFile, err := os.Open(path)
		if err != nil {
			// this repo does not contain a debian package
			continue
		}
		defer ctrlFile.Close()
		ctrl, err := control.ParseControl(
			bufio.NewReader(ctrlFile), path)
		if err != nil {
			// if there is a control file but it cannot be parsed
			// by this tool, we'll attempt to just build it last
			// the control files should get fixed so this
			// is unnecessary.
			out.unparseable = append(out.unparseable, repo.Name())
			continue
		}
		out.ctrlFiles[repo.Name()] = ctrl
		for _, bin := range ctrl.Binaries {
			pkgName := strings.TrimSpace(bin.Package)
			out.pack2repo[pkgName] = repo.Name()
		}
	}
	return out
}

func determineBuildOrder(repos repoMetaData) []string {
	depGraph := tsort.New()
	for repo, ctrl := range repos.ctrlFiles {
		depGraph.AddVertex(repo)
		// Assume everything requires our base-files
		depGraph.AddEdge(repo, "base-files")
		for _, rel := range ctrl.Source.BuildDepends.Relations {
			for _, pos := range rel.Possibilities {
				name := strings.TrimSpace(pos.Name)
				drepo, ok := repos.pack2repo[name]
				if !ok {
					// the dependency is not from
					// a DANOS repository
					continue
				}
				depGraph.AddEdge(repo, drepo)
			}
		}
	}

	sorted, err := depGraph.Sort()
	if err != nil {
		panic(err)
	}

	return append(sorted, repos.unparseable...)
}

func buildRepo(debDir, baseDir, repo, version string) error {
	fmt.Println("Building", repo)
	bldr, err := bpkg.MakeBuilder(
		bpkg.SourceDirectory(
			resolvePath(filepath.Join(baseDir, repo))),
		bpkg.DestinationDirectory(resolvePath(debDir)),
		bpkg.PreferredPackageDirectory(resolvePath(debDir)),
		bpkg.Version(version),
	)
	if err != nil {
		return buildError{repo: repo, err: err}
	}
	defer bldr.Close()
	err = bldr.Build()
	if err != nil {
		return buildError{repo: repo, err: err}
	}
	return nil
}

func buildRepos(repos []string, logDir, debDir, baseDir, version string) error {
	var buildErrs errList
	done := make(chan struct{})
	interrupt := make(chan os.Signal)
	signal.Notify(interrupt, os.Interrupt)
	logf, err := os.OpenFile(filepath.Join(logDir, "failed-builds.log"),
		os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		return err
	}
	defer logf.Close()
	go func() {
		for _, repo := range repos {
			err := teeAndEval(logDir, repo, func() error {
				return buildRepo(debDir, baseDir, repo, version)
			})
			if err != nil {
				buildErrs = append(buildErrs, err)
				fmt.Fprintln(logf, err)
			}
		}
		close(done)
	}()
	select {
	case <-done:
		fmt.Println("finished builds")
	case <-interrupt:
		fmt.Println("interrupt received")
	}
	if len(buildErrs) != 0 {
		return buildErrs
	}
	return nil

}

func teeAndEval(logdir, repo string, fn func() error) error {
	stdout := os.Stdout
	stderr := os.Stderr
	outr, outw, e := os.Pipe()
	if e != nil {
		return e
	}
	os.Stdout = outw
	os.Stderr = outw

	outf, e := os.OpenFile(filepath.Join(logdir, repo+".log"),
		os.O_RDWR|os.O_CREATE, 0755)
	if e != nil {
		return e
	}
	defer outf.Close()

	out := io.MultiWriter(stdout, outf)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		io.Copy(out, outr)
		wg.Done()
	}()

	rval := fn()

	outw.Close()
	os.Stdout = stdout
	os.Stderr = stderr
	wg.Wait()

	return rval
}

func handleError(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	flag.BoolVar(&clone, "clone", false, "Clone all DANOS git repos")
	flag.BoolVar(&build, "build", false, "Build all cloned repos")
	flag.StringVar(&srcDir, "src", "src", "source directory")
	flag.StringVar(&pkgDir, "pkg", "pkg", "package directory")
	flag.StringVar(&logDir, "log", "log", "log directory")
	flag.StringVar(&version, "version", "latest", "version of danos to build for")
}

func main() {
	flag.Parse()
	if clone {
		err := cloneRepos(srcDir)
		handleError(err)
	}

	repos := enumerateBuildableRepos(srcDir)
	buildOrder := determineBuildOrder(repos)

	fmt.Printf("Build order (%d repos): %s\n",
		len(buildOrder), buildOrder)

	if build {
		err := os.MkdirAll(logDir, 0777)
		handleError(err)
		err = buildRepos(buildOrder, logDir, pkgDir, srcDir, version)
		handleError(err)
	}
}
