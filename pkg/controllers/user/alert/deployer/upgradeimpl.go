package deployer

import (
	"context"
	"fmt"
	"reflect"

	"github.com/rancher/rancher/pkg/ref"
	"github.com/rancher/types/apis/core/v1"

	"github.com/rancher/norman/controller"
	alertutil "github.com/rancher/rancher/pkg/controllers/user/alert/common"
	"github.com/rancher/rancher/pkg/controllers/user/systemimage"
	monitorutil "github.com/rancher/rancher/pkg/monitoring"
	"github.com/rancher/types/apis/apps/v1beta2"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	projectv3 "github.com/rancher/types/apis/project.cattle.io/v3"

	"github.com/rancher/rancher/pkg/controllers/user/helm/common"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/types/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
)

var (
	serviceName = "alerting"
)

type alertService struct {
	clusterName        string
	apps               projectv3.AppInterface
	deployments        v1beta2.DeploymentInterface
	oldClusterAlerts   v3.ClusterAlertInterface
	oldProjectAlerts   v3.ProjectAlertInterface
	clusterAlertGroups v3.ClusterAlertGroupInterface
	projectAlertGroups v3.ProjectAlertGroupInterface
	clusterAlertRules  v3.ClusterAlertRuleInterface
	projectAlertRules  v3.ProjectAlertRuleInterface
	projectLister      v3.ProjectLister
	namespaces         v1.NamespaceInterface
	appDeployer        *appDeployer
}

func init() {
	systemimage.RegisterSystemService(serviceName, &alertService{})
}

func (l *alertService) Init(ctx context.Context, cluster *config.UserContext) {
	ad := &appDeployer{
		appsGetter:       cluster.Management.Project,
		namespaces:       cluster.Management.Core.Namespaces(metav1.NamespaceAll),
		secrets:          cluster.Core.Secrets(metav1.NamespaceAll),
		templateVersions: cluster.Management.Management.TemplateVersions(metav1.NamespaceAll),
	}

	l.clusterName = cluster.ClusterName
	l.deployments = cluster.Apps.Deployments("")
	l.oldClusterAlerts = cluster.Management.Management.ClusterAlerts(cluster.ClusterName)
	l.oldProjectAlerts = cluster.Management.Management.ProjectAlerts(metav1.NamespaceAll)
	l.clusterAlertGroups = cluster.Management.Management.ClusterAlertGroups(cluster.ClusterName)
	l.projectAlertGroups = cluster.Management.Management.ProjectAlertGroups(metav1.NamespaceAll)
	l.clusterAlertRules = cluster.Management.Management.ClusterAlertRules(cluster.ClusterName)
	l.projectAlertRules = cluster.Management.Management.ProjectAlertRules(metav1.NamespaceAll)
	l.projectLister = cluster.Management.Management.Projects(cluster.ClusterName).Controller().Lister()
	l.apps = cluster.Management.Project.Apps(metav1.NamespaceAll)
	l.appDeployer = ad

}

func (l *alertService) Version() (string, error) {
	catalogID := settings.SystemMonitoringCatalogID.Get()
	templateVersionID, err := common.ParseExternalID(catalogID)
	if err != nil {
		return "", fmt.Errorf("get system monitor catalog version failed, %v", err)
	}
	return templateVersionID, nil
}

func (l *alertService) Upgrade(currentVersion string) (string, error) {
	newCatalogID := settings.SystemMonitoringCatalogID.Get()

	NewVersion, err := common.ParseExternalID(newCatalogID)
	if currentVersion == NewVersion {
		return currentVersion, nil
	}

	//migrate legacy
	appName, _ := monitorutil.ClusterAlertManagerInfo()
	oldClusterAlert, err := l.oldClusterAlerts.List(metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("get old cluster alert failed, %s", err)
	}

	for _, v := range oldClusterAlert.Items {
		migrationGroupName := fmt.Sprintf("migrate-group-%s", v.Name)
		groupID := alertutil.GetGroupID(l.clusterName, migrationGroupName)

		name := fmt.Sprintf("migrate-%s", v.Name)
		newClusterRule := &v3.ClusterAlertRule{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: l.clusterName,
			},
			Spec: v3.ClusterAlertRuleSpec{
				ClusterName: l.clusterName,
				GroupName:   groupID,
				CommonRuleField: v3.CommonRuleField{
					DisplayName: v.Spec.DisplayName,
					Severity:    v.Spec.Severity,
					TimingField: v3.TimingField{
						GroupIntervalSeconds:  v.Spec.InitialWaitSeconds,
						RepeatIntervalSeconds: v.Spec.RepeatIntervalSeconds,
					},
				},
			},
		}

		if v.Spec.TargetNode != nil {
			newClusterRule.Spec.NodeRule = &v3.NodeRule{
				NodeName:     v.Spec.TargetNode.NodeName,
				Selector:     v.Spec.TargetNode.Selector,
				Condition:    v.Spec.TargetNode.Condition,
				MemThreshold: v.Spec.TargetNode.MemThreshold,
				CPUThreshold: v.Spec.TargetNode.CPUThreshold,
			}
		}

		if v.Spec.TargetEvent != nil {
			newClusterRule.Spec.EventRule = &v3.EventRule{
				EventType:    v.Spec.TargetEvent.EventType,
				ResourceKind: v.Spec.TargetEvent.ResourceKind,
			}
		}

		if v.Spec.TargetSystemService != nil {
			newClusterRule.Spec.SystemServiceRule = &v3.SystemServiceRule{
				Condition: v.Spec.TargetSystemService.Condition,
			}
		}

		oldClusterRule, err := l.clusterAlertRules.Get(newClusterRule.Name, metav1.GetOptions{})
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return "", fmt.Errorf("migrate %s:%s failed, get alert rule failed, %v", v.Namespace, v.Name, err)
			}

			if _, err = l.clusterAlertRules.Create(newClusterRule); err != nil && !apierrors.IsAlreadyExists(err) {
				return "", fmt.Errorf("migrate %s:%s failed, create alert rule failed, %v", v.Namespace, v.Name, err)
			}
		} else {
			updatedClusterRule := oldClusterRule.DeepCopy()
			updatedClusterRule.Spec = newClusterRule.Spec
			if _, err := l.clusterAlertRules.Update(updatedClusterRule); err != nil {
				return "", fmt.Errorf("migrate %s:%s failed, update alert rule failed, %v", v.Namespace, v.Name, err)
			}
		}

		legacyGroup := &v3.ClusterAlertGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      migrationGroupName,
				Namespace: l.clusterName,
			},
			Spec: v3.ClusterGroupSpec{
				ClusterName: l.clusterName,
				CommonGroupField: v3.CommonGroupField{
					DisplayName: "Migrate group",
					Description: "Migrate alert from last version",
				},
				Recipients: v.Spec.Recipients,
			},
		}

		legacyGroup, err = l.clusterAlertGroups.Create(legacyGroup)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("migrate failed, create alert group %s:%s failed, %v", l.clusterName, migrationGroupName, err)
		}
	}

	oldProjectAlert, err := l.oldProjectAlerts.List(metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("get old project alert failed, %s", err)
	}

	oldProjectAlertGroup := make(map[string][]*v3.ProjectAlert)
	for _, v := range oldProjectAlert.Items {
		if controller.ObjectInCluster(l.clusterName, v) {
			oldProjectAlertGroup[v.Spec.ProjectName] = append(oldProjectAlertGroup[v.Spec.ProjectName], &v)
		}
	}

	for projectID, oldAlerts := range oldProjectAlertGroup {
		_, projectName := ref.Parse(projectID)

		for _, v := range oldAlerts {
			migrationGroupName := fmt.Sprintf("migrate-group-%s", v.Name)
			groupID := alertutil.GetGroupID(projectName, migrationGroupName)

			migrationRuleName := fmt.Sprintf("migrate-rule-%s", v.Name)
			newProjectRule := &v3.ProjectAlertRule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      migrationRuleName,
					Namespace: projectName,
				},
				Spec: v3.ProjectAlertRuleSpec{
					ProjectName: projectName,
					GroupName:   groupID,
					CommonRuleField: v3.CommonRuleField{
						DisplayName: v.Spec.DisplayName,
						Severity:    v.Spec.Severity,
						TimingField: v3.TimingField{
							GroupIntervalSeconds:  v.Spec.InitialWaitSeconds,
							RepeatIntervalSeconds: v.Spec.RepeatIntervalSeconds,
						},
					},
				},
			}

			if v.Spec.TargetPod != nil {
				newProjectRule.Spec.PodRule = &v3.PodRule{
					PodName:                v.Spec.TargetPod.PodName,
					Condition:              v.Spec.TargetPod.Condition,
					RestartTimes:           v.Spec.TargetPod.RestartTimes,
					RestartIntervalSeconds: v.Spec.TargetPod.RestartIntervalSeconds,
				}
			}

			if v.Spec.TargetWorkload != nil {
				newProjectRule.Spec.WorkloadRule = &v3.WorkloadRule{
					WorkloadID:          v.Spec.TargetWorkload.WorkloadID,
					Selector:            v.Spec.TargetWorkload.Selector,
					AvailablePercentage: v.Spec.TargetWorkload.AvailablePercentage,
				}
			}

			oldProjectRule, err := l.projectAlertRules.GetNamespaced(projectName, newProjectRule.Name, metav1.GetOptions{})
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return "", fmt.Errorf("migrate %s:%s failed, get alert rule failed, %v", v.Namespace, v.Name, err)
				}

				if _, err = l.projectAlertRules.Create(newProjectRule); err != nil && !apierrors.IsAlreadyExists(err) {
					return "", fmt.Errorf("migrate %s:%s failed, create alert rule failed, %v", v.Namespace, v.Name, err)
				}
			} else {
				updatedProjectRule := oldProjectRule.DeepCopy()
				updatedProjectRule.Spec = newProjectRule.Spec
				if _, err := l.projectAlertRules.Update(updatedProjectRule); err != nil {
					return "", fmt.Errorf("migrate %s:%s failed, update alert rule failed, %v", v.Namespace, v.Name, err)
				}
			}

			legacyGroup := &v3.ProjectAlertGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      migrationGroupName,
					Namespace: projectName,
				},
				Spec: v3.ProjectGroupSpec{
					ProjectName: projectID,
					CommonGroupField: v3.CommonGroupField{
						DisplayName: "Migrate group",
						Description: "Migrate alert from last version",
					},
					Recipients: v.Spec.Recipients,
				},
			}

			legacyGroup, err = l.projectAlertGroups.Create(legacyGroup)
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return "", fmt.Errorf("create migrate alert group %s:%s failed, %v", legacyGroup.Namespace, legacyGroup.Name, err)
			}
		}

	}

	//clean up
	set := fields.Set{
		"metadata.namespace": l.clusterName,
	}
	if err := l.oldClusterAlerts.DeleteCollection(&metav1.DeleteOptions{}, metav1.ListOptions{
		FieldSelector: set.AsSelector().String(),
	}); err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("migrate failed, delete old cluster alert in namespace %s failded, %v", l.clusterName, err)
	}

	for projectName := range oldProjectAlertGroup {
		if err := l.oldProjectAlerts.DeleteCollection(&metav1.DeleteOptions{}, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.namespace=%s", projectName),
		}); err != nil && !apierrors.IsNotFound(err) {
			return "", fmt.Errorf("migrate failed, delete old project alert in namespace %s failded, %v", projectName, err)
		}
	}
	//upgrade old app
	defaultSystemProjects, err := l.projectLister.List(metav1.NamespaceAll, labels.Set(systemProjectLabel).AsSelector())
	if err != nil {
		return "", fmt.Errorf("list system project failed, %v", err)
	}

	if len(defaultSystemProjects) == 0 {
		return "", fmt.Errorf("get system project failed")
	}

	systemProject := defaultSystemProjects[0]
	if systemProject == nil {
		return "", fmt.Errorf("get system project failed")
	}

	app, err := l.apps.GetNamespaced(systemProject.Name, appName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return NewVersion, nil
		}
		return "", fmt.Errorf("get app %s:%s failed, %v", systemProject.Name, appName, err)
	}
	newApp := app.DeepCopy()
	newApp.Spec.ExternalID = newCatalogID

	if !reflect.DeepEqual(newApp, app) {
		if _, err = l.apps.Update(newApp); err != nil {
			return "", fmt.Errorf("update app %s:%s failed, %v", app.Namespace, app.Name, err)
		}
	}

	return NewVersion, nil
}