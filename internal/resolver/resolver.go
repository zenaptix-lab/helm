/*
Copyright The Helm Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resolver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/pkg/errors"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/gates"
	"helm.sh/helm/v3/pkg/gitutil"
	"helm.sh/helm/v3/pkg/helmpath"
	"helm.sh/helm/v3/pkg/provenance"
	"helm.sh/helm/v3/pkg/repo"
)

const FeatureGateOCI = gates.Gate("HELM_EXPERIMENTAL_OCI")

var gitGetRefs = gitutil.GetRefs

// Resolver resolves dependencies from semantic version ranges to a particular version.
type Resolver struct {
	chartpath string
	cachepath string
}

// New creates a new resolver for a given chart and a given helm home.
func New(chartpath, cachepath string) *Resolver {
	return &Resolver{
		chartpath: chartpath,
		cachepath: cachepath,
	}
}

// Resolve resolves dependencies and returns a lock file with the resolution.
func (r *Resolver) Resolve(reqs []*chart.Dependency, repoNames map[string]string) (*chart.Lock, error) {

	// Now we clone the dependencies, locking as we go.
	locked := make([]*chart.Dependency, len(reqs))
	missing := []string{}
	for i, d := range reqs {
		if d.Repository == "" {
			// Local chart subfolder
			if _, err := GetLocalPath(filepath.Join("charts", d.Name), r.chartpath); err != nil {
				return nil, err
			}

			locked[i] = &chart.Dependency{
				Name:       d.Name,
				Repository: "",
				Version:    d.Version,
			}
			continue
		}
		if strings.HasPrefix(d.Repository, "file://") {

			chartpath, err := GetLocalPath(d.Repository, r.chartpath)
			if err != nil {
				return nil, err
			}

			// The version of the chart locked will be the version of the chart
			// currently listed in the file system within the chart.
			ch, err := loader.LoadDir(chartpath)
			if err != nil {
				return nil, err
			}

			locked[i] = &chart.Dependency{
				Name:       d.Name,
				Repository: d.Repository,
				Version:    ch.Metadata.Version,
			}
			continue
		}


		if strings.HasPrefix(d.Repository, "git:") {
			refs, err := gitGetRefs(strings.TrimPrefix(d.Repository, "git:"))

			if err != nil {
				return nil, err
			}

			_, found := refs[d.Version]

			if !found {
				return nil, fmt.Errorf(`dependency %q is missing git branch or tag: %s.
			When using a "git:" type repository, the "version" should be a valid branch or tag name`, d.Name, d.Version)
			}

			locked[i] = &chart.Dependency{
				Name:       d.Name,
				Repository: d.Repository,
				Version:    d.Version,
			}
			continue
		}

		constraint, err := semver.NewConstraint(d.Version)
		if err != nil {
			return nil, errors.Wrapf(err, "dependency %q has an invalid version/constraint format", d.Name)
		}

		repoName := repoNames[d.Name]
		// if the repository was not defined, but the dependency defines a repository url, bypass the cache
		if repoName == "" && d.Repository != "" {
			locked[i] = &chart.Dependency{
				Name:       d.Name,
				Repository: d.Repository,
				Version:    d.Version,
			}
			continue
		}

		var vs repo.ChartVersions
		var version string
		var ok bool
		found := true
		if !strings.HasPrefix(d.Repository, "oci://") {
			repoIndex, err := repo.LoadIndexFile(filepath.Join(r.cachepath, helmpath.CacheIndexFile(repoName)))
			if err != nil {
				return nil, errors.Wrapf(err, "no cached repository for %s found. (try 'helm repo update')", repoName)
			}

			vs, ok = repoIndex.Entries[d.Name]
			if !ok {
				return nil, errors.Errorf("%s chart not found in repo %s", d.Name, d.Repository)
			}
			found = false
		} else {
			version = d.Version
			if !FeatureGateOCI.IsEnabled() {
				return nil, errors.Wrapf(FeatureGateOCI.Error(),
					"repository %s is an OCI registry", d.Repository)
			}
		}

		locked[i] = &chart.Dependency{
			Name:       d.Name,
			Repository: d.Repository,
			Version:    version,
		}
		// The version are already sorted and hence the first one to satisfy the constraint is used
		for _, ver := range vs {
			v, err := semver.NewVersion(ver.Version)
			if err != nil || len(ver.URLs) == 0 {
				// Not a legit entry.
				continue
			}
			if constraint.Check(v) {
				found = true
				locked[i].Version = v.Original()
				break
			}
		}

		if !found {
			missing = append(missing, d.Name)
		}
	}
	if len(missing) > 0 {
		return nil, errors.Errorf("can't get a valid version for repositories %s. Try changing the version constraint in Chart.yaml", strings.Join(missing, ", "))
	}

	digest, err := HashReq(reqs, locked)
	if err != nil {
		return nil, err
	}

	return &chart.Lock{
		Generated:    time.Now(),
		Digest:       digest,
		Dependencies: locked,
	}, nil
}

// HashReq generates a hash of the dependencies.
//
// This should be used only to compare against another hash generated by this
// function.
func HashReq(req, lock []*chart.Dependency) (string, error) {
	data, err := json.Marshal([2][]*chart.Dependency{req, lock})
	if err != nil {
		return "", err
	}
	s, err := provenance.Digest(bytes.NewBuffer(data))
	return "sha256:" + s, err
}

// HashV2Req generates a hash of requirements generated in Helm v2.
//
// This should be used only to compare against another hash generated by the
// Helm v2 hash function. It is to handle issue:
// https://github.com/helm/helm/issues/7233
func HashV2Req(req []*chart.Dependency) (string, error) {
	dep := make(map[string][]*chart.Dependency)
	dep["dependencies"] = req
	data, err := json.Marshal(dep)
	if err != nil {
		return "", err
	}
	s, err := provenance.Digest(bytes.NewBuffer(data))
	return "sha256:" + s, err
}

// GetLocalPath generates absolute local path when use
// "file://" in repository of dependencies
func GetLocalPath(repo, chartpath string) (string, error) {
	var depPath string
	var err error
	p := strings.TrimPrefix(repo, "file://")

	// root path is absolute
	if strings.HasPrefix(p, "/") {
		if depPath, err = filepath.Abs(p); err != nil {
			return "", err
		}
	} else {
		depPath = filepath.Join(chartpath, p)
	}

	if _, err = os.Stat(depPath); os.IsNotExist(err) {
		return "", errors.Errorf("directory %s not found", depPath)
	} else if err != nil {
		return "", err
	}

	return depPath, nil
}
