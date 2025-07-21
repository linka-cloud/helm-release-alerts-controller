/*
Copyright 2025.

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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	url2 "net/url"
	"os"
	"time"

	"github.com/blang/semver/v4"
	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/go-openapi/strfmt"
	apiv2 "github.com/prometheus/alertmanager/api/v2/client"
	"github.com/prometheus/alertmanager/api/v2/client/alert"
	"github.com/prometheus/alertmanager/api/v2/models"
	"helm.sh/helm/v4/pkg/repo"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var requeueAfter = 5 * time.Minute

const (
	AlertName     = "HelmReleaseUpdateAvailable"
	AlertSeverity = "warning"
)

// HelmReleaseReconciler reconciles a HelmRelease object
type HelmReleaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	API    *apiv2.AlertmanagerAPI
}

// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases/status,verbs=get
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories,verbs=get;list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories/status,verbs=get
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts,verbs=get;list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts/status,verbs=get

func (r *HelmReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	var filters = []string{
		"alertname=" + AlertName,
		"severity=" + AlertSeverity,
		"apiVersion=helm.toolkit.fluxcd.io/v2",
		"kind=HelmRelease",
		"namespace=" + req.Namespace,
		"name=" + req.Name,
	}
	log.V(6).Info("retrieving alerts")
	alerts, err := r.API.Alert.GetAlerts(&alert.GetAlertsParams{Context: ctx, Filter: filters})
	if err != nil {
		return ctrl.Result{}, err
	}
	var hr helmv2.HelmRelease
	log.V(6).Info("retrieving helm release")
	if err := r.Get(ctx, req.NamespacedName, &hr); err != nil {
		if !apierrors.IsNotFound(err) {
			log.V(6).Info("helm release not found")
			return ctrl.Result{}, err
		}
		if len(alerts.GetPayload()) == 0 {
			log.V(6).Info("no alerts to delete")
			return ctrl.Result{}, nil
		}
		return r.expireAlerts(ctx, alerts.GetPayload())
	}
	if hr.Spec.Chart == nil || hr.Spec.Chart.Spec.SourceRef.Kind != "HelmRepository" {
		log.Info("no helm repository, skipping")
		return ctrl.Result{}, nil
	}
	if hr.Status.LastAttemptedRevision == "" {
		log.Info("no last attempted revision, skipping")
		return ctrl.Result{}, nil
	}
	if hr.Spec.Chart.Spec.SourceRef.Namespace == "" {
		hr.Spec.Chart.Spec.SourceRef.Namespace = req.Namespace
	}
	cver, err := semver.ParseTolerant(hr.Status.LastAttemptedRevision)
	if err != nil {
		log.Info("non semver version, skipping", "version", hr.Status.LastAttemptedRevision)
		return ctrl.Result{}, nil
	}
	var hrepo sourcev1.HelmRepository
	log.V(6).Info("retrieving helm repository")
	if err := r.Get(ctx, types.NamespacedName{Name: hr.Spec.Chart.Spec.SourceRef.Name, Namespace: hr.Spec.Chart.Spec.SourceRef.Namespace}, &hrepo); err != nil {
		if apierrors.IsNotFound(err) {
			log.V(6).Info("helm repository not found")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if hrepo.Status.URL == "" {
		log.V(6).Info("helm repository not ready")
		return ctrl.Result{}, nil
	}
	log.V(6).Info("retrieving helm repository index")
	index, err := getIndex(ctx, hrepo.Status.Artifact.URL)
	if err != nil {
		return ctrl.Result{}, err
	}
	c, ok := index.Entries[hr.Spec.Chart.Spec.Chart]
	if !ok {
		log.Info("chart not found in index")
		return ctrl.Result{}, nil
	}
	last := cver
	var (
		aver  semver.Version
		alast semver.Version
	)
	for _, v := range c {
		ver, err := semver.ParseTolerant(v.Version)
		if err != nil {
			log.V(6).Info("non semver version, skipping", "version", v.Version)
			continue
		}
		if len(ver.Pre) != 0 {
			continue
		}
		av, err := semver.ParseTolerant(v.AppVersion)
		if err != nil {
			log.V(6).Info("non semver app version, skipping", "version", v.AppVersion)
		}
		switch {
		case ver.EQ(cver):
			aver = av
		case ver.GT(last):
			last = ver
			alast = av
		}
	}
	if last.EQ(cver) {
		log.Info("release up to date", "version", cver)
		return r.expireAlerts(ctx, alerts.GetPayload())
	}
	log.Info("new version available", "current", cver.String(), "last", last.String())
	if len(alerts.GetPayload()) == 0 {
		log.V(6).Info("creating alert")
		if _, err := r.API.Alert.PostAlerts(&alert.PostAlertsParams{Context: ctx, Alerts: models.PostableAlerts{createAlert(hr, cver, last, aver, alast)}}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	var pa models.PostableAlerts
	for _, v := range alerts.GetPayload() {
		if v.Labels["current"] == cver.String() && v.Labels["last"] == last.String() && (v.StartsAt != nil && time.Now().Before(time.Time(*v.StartsAt).Add(requeueAfter))) {
			log.V(6).Info("alert up to date")
			continue
		}
		pa = append(pa, createAlert(hr, cver, last, aver, alast))
	}
	if len(pa) == 0 {
		log.Info("alert already exists")
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	log.Info("updating alert", "current", cver.String(), "last", last.String())
	if _, err := r.API.Alert.PostAlerts(&alert.PostAlertsParams{Context: ctx, Alerts: pa}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HelmReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&helmv2.HelmRelease{}).
		Named("helmreleases").
		Complete(r)
}

func (r *HelmReleaseReconciler) expireAlerts(ctx context.Context, alerts models.GettableAlerts) (ctrl.Result, error) {
	if len(alerts) == 0 {
		return ctrl.Result{}, nil
	}
	var as models.PostableAlerts
	for _, v := range alerts {
		as = append(as, &models.PostableAlert{
			StartsAt:    *v.StartsAt,
			EndsAt:      strfmt.DateTime(time.Now()),
			Annotations: v.Annotations,
			Alert: models.Alert{
				Labels: v.Labels,
			},
		})
	}
	if _, err := r.API.Alert.PostAlerts(&alert.PostAlertsParams{Context: ctx, Alerts: as}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func getIndex(ctx context.Context, url string) (*repo.IndexFile, error) {
	u, err := url2.Parse(url)
	if err != nil {
		return nil, err
	}
	if e := os.Getenv("SOURCE_ENDPOINT"); e != "" {
		u.Host = e
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get index: %s", res.Status)
	}
	var index repo.IndexFile
	if err := json.NewDecoder(res.Body).Decode(&index); err != nil {
		return nil, err
	}
	return &index, nil
}

func createAlert(hr helmv2.HelmRelease, cver, clast, aver, alast semver.Version) *models.PostableAlert {
	return &models.PostableAlert{
		StartsAt: strfmt.DateTime(time.Now()),
		Annotations: map[string]string{
			"summary": fmt.Sprintf("%s/%s New chart version available: %s (app: %s), Current chat version: %s (app: %s)", hr.Namespace, hr.Name, clast.String(), alast.String(), cver.String(), aver.String()),
		},
		Alert: models.Alert{
			Labels: models.LabelSet{
				"alertname":    AlertName,
				"severity":     AlertSeverity,
				"apiVersion":   hr.APIVersion,
				"kind":         hr.Kind,
				"namespace":    hr.Namespace,
				"name":         hr.Name,
				"chartCurrent": cver.String(),
				"chartLast":    clast.String(),
				"appCurrent":   aver.String(),
				"appLast":      alast.String(),
			},
		},
	}
}
