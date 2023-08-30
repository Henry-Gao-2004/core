/*
Copyright 2023 The Kubebb Authors.

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

package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kubebb/core/api/v1alpha1"
	"github.com/kubebb/core/pkg/helm"
	"github.com/kubebb/core/pkg/utils"
)

var _ IWatcher = (*OCIWatcher)(nil)

func NewOCIWatcher(
	instance *v1alpha1.Repository,
	c client.Client,
	ctx context.Context,
	logger logr.Logger,
	duration time.Duration,
	cancel context.CancelFunc,
	scheme *runtime.Scheme,
	fm map[string]v1alpha1.FilterCond,
) IWatcher {
	result := &OCIWatcher{
		instance:  instance,
		logger:    logger,
		duration:  duration,
		cancel:    cancel,
		scheme:    scheme,
		repoName:  instance.NamespacedName(),
		filterMap: fm,
	}

	// Common Action in the watcher needs client and context to function
	result.c = c
	result.ctx = ctx
	return result
}

type OCIWatcher struct {
	CommonAction
	cancel   context.CancelFunc
	instance *v1alpha1.Repository
	duration time.Duration
	repoName string

	logger    logr.Logger
	scheme    *runtime.Scheme
	filterMap map[string]v1alpha1.FilterCond
}

func (c *OCIWatcher) Start() error {
	entry, err := Start(c.ctx, c.instance, c.duration, c.repoName, c.c, c.logger)
	if err != nil {
		return err
	}
	entry.Name = c.repoName
	entry.URL = c.instance.Spec.URL

	if err := helm.RepoAdd(c.ctx, c.logger, entry, c.duration/2); err != nil {
		c.logger.Error(err, "Failed to add repository")
		return err
	}

	go wait.Until(c.Poll, c.duration, c.ctx.Done())
	return nil
}

func (c *OCIWatcher) Stop() {
	c.logger.Info("Delete Or Update Repository, stop watcher")
	if err := helm.RepoRemove(c.ctx, c.logger, c.repoName); err != nil {
		c.logger.Error(err, "Failed to remove repository")
	}
	c.cancel()
}

// Poll the components
func (c *OCIWatcher) Poll() {
	c.logger.Info("OCI poll")
	now := metav1.Now()
	readyCond := getReadyCond(now)
	syncCond := getSyncCond(now)

	if err := helm.RepoUpdate(c.ctx, c.logger, c.repoName, c.duration/2); err != nil {
		c.logger.Error(err, "Failed to update repository")
		return
	}
	entryName := utils.GetOCIEntryName(c.instance.Spec.URL)
	cfg, err := ctrl.GetConfig()
	if err != nil {
		c.logger.Error(err, "Cannot get config")
		return
	}
	ns := c.instance.GetNamespace()
	getter := genericclioptions.ConfigFlags{
		APIServer:   &cfg.Host,
		CAFile:      &cfg.CAFile,
		BearerToken: &cfg.BearerToken,
		Namespace:   &ns,
	}

	latest, all, err := helm.GetOCIRepoCharts(c.ctx, &getter, c.c, c.logger, ns, c.instance)
	if err != nil {
		c.logger.Error(err, "Cannot get oci repo charts")
		return
	}

	item := v1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%s", c.instance.GetName(), entryName),
			Namespace: c.instance.GetNamespace(),
			Labels: map[string]string{
				v1alpha1.ComponentRepositoryLabel: c.instance.GetName(),
			},
		},
		Status: v1alpha1.ComponentStatus{
			RepositoryRef: &v1.ObjectReference{
				Kind:       c.instance.Kind,
				Name:       c.instance.GetName(),
				Namespace:  c.instance.GetNamespace(),
				UID:        c.instance.GetUID(),
				APIVersion: c.instance.APIVersion,
			},
			Name:        entryName,
			DisplayName: latest.Annotations[v1alpha1.DisplayNameAnnotationKey],
			Versions:    make([]v1alpha1.ComponentVersion, 0),
			Maintainers: make([]v1alpha1.Maintainer, 0),
		},
	}

	maintainers := make(map[string]v1alpha1.Maintainer)
	for _, m := range latest.Maintainers {
		if _, ok := maintainers[m.Name]; !ok {
			maintainers[m.Name] = v1alpha1.Maintainer{
				Name:  m.Name,
				Email: m.Email,
				URL:   m.URL,
			}
		}
	}
	item.Status.Versions = make([]v1alpha1.ComponentVersion, 0)
	filterVersionIndices, keep := v1alpha1.Match(c.filterMap, v1alpha1.Filter{Name: entryName, Versions: all})
	if keep {
		for _, idx := range filterVersionIndices {
			version := all[idx]
			item.Status.Versions = append(item.Status.Versions, v1alpha1.ComponentVersion{
				Annotations: version.Annotations,
				Version:     version.Version,
				AppVersion:  version.AppVersion,
				CreatedAt:   metav1.NewTime(version.Created),
				Digest:      version.Digest,
				UpdatedAt:   metav1.Now(),
				Deprecated:  version.Deprecated,
			})
		}
	}
	keywords := latest.Keywords
	if r := c.instance.Spec.KeywordLenLimit; r > 0 && len(keywords) > r {
		keywords = keywords[:r]
	}
	item.Status.Description = latest.Description
	item.Status.Home = latest.Home
	item.Status.Icon = latest.Icon
	item.Status.Keywords = keywords
	item.Status.Sources = latest.Sources

	for _, m := range maintainers {
		item.Status.Maintainers = append(item.Status.Maintainers, m)
	}

	_ = controllerutil.SetOwnerReference(c.instance, &item, c.scheme)

	c.logger.Info("create component", "info", item)
	if err := c.Create(&item); err != nil && !errors.IsAlreadyExists(err) {
		c.logger.Error(err, "failed to create component")
	} else {
		c.logger.Info("Successfully created component", "Component.Name", item.GetName(), "Component.Namespace", item.GetNamespace())
	}

	updateRepository(c.ctx, c.instance, c.c, c.logger, readyCond, syncCond)
}
