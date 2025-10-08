package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func listPersistentVolumes(clientset *kubernetes.Clientset, storageClass string) ([]corev1.PersistentVolume, error) {
	pvs, err := clientset.CoreV1().PersistentVolumes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list persistent volumes: %w", err)
	}

	// If no storage class filter is specified, return all PVs
	if storageClass == "" {
		return pvs.Items, nil
	}

	// Filter by storage class
	var filtered []corev1.PersistentVolume
	for _, pv := range pvs.Items {
		if pv.Spec.StorageClassName == storageClass {
			filtered = append(filtered, pv)
		}
	}

	return filtered, nil
}

func migratePersistentVolume(clientset *kubernetes.Clientset, pvName string) error {
	ctx := context.TODO()

	// Step 1: Find the PV and its associated PVC
	fmt.Printf("Step 1: Finding PV %s and its PVC...\n", pvName)
	pv, err := clientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get PV: %w", err)
	}

	if pv.Spec.ClaimRef == nil {
		return fmt.Errorf("PV %s is not bound to any PVC", pvName)
	}

	pvcName := pv.Spec.ClaimRef.Name
	pvcNamespace := pv.Spec.ClaimRef.Namespace
	fmt.Printf("Found PVC: %s/%s\n", pvcNamespace, pvcName)

	pvc, err := clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get PVC: %w", err)
	}

	// Step 2: Find the deployment or statefulset
	fmt.Printf("Step 2: Finding deployments and statefulsets using PVC...\n")
	var deploymentsToScale []string
	var statefulsetsToScale []string
	var deploymentReplicas = make(map[string]int32)
	var statefulsetReplicas = make(map[string]int32)
	var found bool

	// Check deployments
	deployments, err := clientset.AppsV1().Deployments(pvcNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list deployments: %w", err)
	}

	for _, deploy := range deployments.Items {
		for _, volume := range deploy.Spec.Template.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvcName {
				deploymentsToScale = append(deploymentsToScale, deploy.Name)
				deploymentReplicas[deploy.Name] = *deploy.Spec.Replicas
				fmt.Printf("Found Deployment: %s (replicas: %d)\n", deploy.Name, *deploy.Spec.Replicas)
				found = true
				break
			}
		}
	}

	// Check statefulsets
	statefulsets, err := clientset.AppsV1().StatefulSets(pvcNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list statefulsets: %w", err)
	}

	for _, sts := range statefulsets.Items {
		foundInVolumes := false
		foundInTemplates := false

		// Check volumes
		for _, volume := range sts.Spec.Template.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvcName {
				foundInVolumes = true
				break
			}
		}

		// Check volumeClaimTemplates
		// StatefulSet PVC naming: <volumeClaimTemplate.Name>-<statefulset.Name>-<ordinal>
		for _, vct := range sts.Spec.VolumeClaimTemplates {
			// Check if pvcName matches the pattern: vct.Name-sts.Name-*
			templatePrefix := vct.Name + "-" + sts.Name + "-"
			if len(pvcName) >= len(templatePrefix) && pvcName[:len(templatePrefix)] == templatePrefix {
				foundInTemplates = true
				break
			}
		}

		if foundInVolumes || foundInTemplates {
			statefulsetsToScale = append(statefulsetsToScale, sts.Name)
			statefulsetReplicas[sts.Name] = *sts.Spec.Replicas
			if foundInVolumes && foundInTemplates {
				fmt.Printf("Found StatefulSet: %s (replicas: %d) using both volumes and volumeClaimTemplates\n", sts.Name, *sts.Spec.Replicas)
			} else if foundInVolumes {
				fmt.Printf("Found StatefulSet: %s (replicas: %d) using volumes\n", sts.Name, *sts.Spec.Replicas)
			} else {
				fmt.Printf("Found StatefulSet: %s (replicas: %d) using volumeClaimTemplates\n", sts.Name, *sts.Spec.Replicas)
			}
			found = true
		}
	}

	if !found {
		return fmt.Errorf("no deployment or statefulset found using PVC %s", pvcName)
	}

	// Step 3: Scale down the workloads to 0
	fmt.Printf("Step 3: Scaling down workloads to 0...\n")
	replicas := int32(0)

	for _, deployName := range deploymentsToScale {
		fmt.Printf("Scaling down deployment %s...\n", deployName)
		deployment, err := clientset.AppsV1().Deployments(pvcNamespace).Get(ctx, deployName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get deployment %s: %w", deployName, err)
		}
		deployment.Spec.Replicas = &replicas
		_, err = clientset.AppsV1().Deployments(pvcNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to scale down deployment %s: %w", deployName, err)
		}
	}

	for _, stsName := range statefulsetsToScale {
		fmt.Printf("Scaling down statefulset %s...\n", stsName)
		statefulset, err := clientset.AppsV1().StatefulSets(pvcNamespace).Get(ctx, stsName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get statefulset %s: %w", stsName, err)
		}
		statefulset.Spec.Replicas = &replicas
		_, err = clientset.AppsV1().StatefulSets(pvcNamespace).Update(ctx, statefulset, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to scale down statefulset %s: %w", stsName, err)
		}
	}

	// Wait for pods to terminate
	fmt.Println("Waiting for pods to terminate...")
	time.Sleep(10 * time.Second)

	// Step 4: Create new PVC with "-migrate" suffix using default storage class
	fmt.Printf("Step 4: Creating new PVC %s-migrate with default storage class...\n", pvcName)
	newPVCName := pvcName + "-migrate"
	newPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newPVCName,
			Namespace: pvcNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: pvc.Spec.AccessModes,
			Resources:   pvc.Spec.Resources,
			// Use default storage class by setting to nil
			StorageClassName: nil,
		},
	}

	_, err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Create(ctx, newPVC, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create new PVC: %w", err)
	}
	fmt.Printf("Created new PVC: %s\n", newPVCName)

	// Start a goroutine to update the PV's reclaim policy once it's provisioned
	go func() {
		fmt.Println("Monitoring for new PV to update reclaim policy...")
		for i := 0; i < 60; i++ {
			pvcCheck, err := clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(context.TODO(), newPVCName, metav1.GetOptions{})
			if err == nil && pvcCheck.Status.Phase == corev1.ClaimBound && pvcCheck.Spec.VolumeName != "" {
				pvName := pvcCheck.Spec.VolumeName
				fmt.Printf("Updating PV %s reclaim policy to Retain...\n", pvName)
				pv, err := clientset.CoreV1().PersistentVolumes().Get(context.TODO(), pvName, metav1.GetOptions{})
				if err == nil {
					pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
					_, err = clientset.CoreV1().PersistentVolumes().Update(context.TODO(), pv, metav1.UpdateOptions{})
					if err == nil {
						fmt.Println("Successfully updated reclaim policy to Retain")
					}
				}
				return
			}
			time.Sleep(2 * time.Second)
		}
	}()

	// Step 5: Create migration pod
	fmt.Printf("Step 5: Creating migration pod...\n")
	migrationPodName := pvcName + "-migration"
	migrationPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationPodName,
			Namespace: pvcNamespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "migration",
					Image:   "busybox",
					Command: []string{"/bin/sh", "-c", "cp -ar /old-data/. /new-data/ && echo 'Migration completed'"},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "old-pvc",
							MountPath: "/old-data",
						},
						{
							Name:      "new-pvc",
							MountPath: "/new-data",
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "old-pvc",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
				{
					Name: "new-pvc",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: newPVCName,
						},
					},
				},
			},
		},
	}

	_, err = clientset.CoreV1().Pods(pvcNamespace).Create(ctx, migrationPod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create migration pod: %w", err)
	}

	// Wait for migration pod to complete
	fmt.Println("Waiting for migration to complete...")
	for i := 0; i < 300; i++ { // Wait up to 10 minutes
		pod, err := clientset.CoreV1().Pods(pvcNamespace).Get(ctx, migrationPodName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get migration pod: %w", err)
		}
		if pod.Status.Phase == corev1.PodSucceeded {
			fmt.Println("Migration completed successfully")
			break
		}
		if pod.Status.Phase == corev1.PodFailed {
			return fmt.Errorf("migration pod failed")
		}
		time.Sleep(2 * time.Second)
	}

	// Delete migration pod
	fmt.Println("Deleting migration pod...")
	err = clientset.CoreV1().Pods(pvcNamespace).Delete(ctx, migrationPodName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete migration pod: %w", err)
	}

	// Wait for migration pod to be deleted
	fmt.Println("Waiting for migration pod to be deleted...")
	for i := 0; i < 30; i++ {
		_, err := clientset.CoreV1().Pods(pvcNamespace).Get(ctx, migrationPodName, metav1.GetOptions{})
		if err != nil {
			// Pod is deleted when we get a "not found" error
			fmt.Println("Migration pod deleted successfully")
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Step 6: Update PVCs to swap PV associations
	fmt.Printf("Step 6: Swapping PV associations...\n")

	// Get the new PV that was created for the new PVC
	newPVC, err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, newPVCName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get new PVC: %w", err)
	}
	newPVName := newPVC.Spec.VolumeName

	// Remove finalizers from both PVCs before deletion
	fmt.Println("Removing finalizers from PVCs...")
	pvc, err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get old PVC: %w", err)
	}
	pvc.Finalizers = []string{}
	_, err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Update(ctx, pvc, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove finalizers from old PVC: %w", err)
	}

	newPVC, err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, newPVCName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get new PVC: %w", err)
	}
	newPVC.Finalizers = []string{}
	_, err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Update(ctx, newPVC, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to remove finalizers from new PVC: %w", err)
	}

	// Delete both PVCs (this will release the PVs)
	fmt.Println("Deleting PVCs...")
	err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Delete(ctx, pvcName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete old PVC: %w", err)
	}
	err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Delete(ctx, newPVCName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete new PVC: %w", err)
	}

	// Wait for PVCs to be deleted
	fmt.Println("Waiting for PVCs to be deleted...")
	for i := 0; i < 60; i++ { // Wait up to 2 minutes
		_, err1 := clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, pvcName, metav1.GetOptions{})
		_, err2 := clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Get(ctx, newPVCName, metav1.GetOptions{})

		// Both PVCs should return "not found" errors
		if err1 != nil && err2 != nil {
			fmt.Println("Both PVCs deleted successfully")
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Update PVs to Released/Available state and remove claimRef
	fmt.Println("Updating PVs...")
	oldPV, err := clientset.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get old PV: %w", err)
	}
	oldPV.Spec.ClaimRef = nil
	_, err = clientset.CoreV1().PersistentVolumes().Update(ctx, oldPV, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update old PV: %w", err)
	}

	newPV, err := clientset.CoreV1().PersistentVolumes().Get(ctx, newPVName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get new PV: %w", err)
	}
	newPV.Spec.ClaimRef = nil
	_, err = clientset.CoreV1().PersistentVolumes().Update(ctx, newPV, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update new PV: %w", err)
	}

	time.Sleep(2 * time.Second)

	// Recreate PVCs with swapped PV bindings
	fmt.Println("Recreating PVCs with swapped bindings...")

	// Create the migrate PVC pointing to the old PV
	migratePVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newPVCName,
			Namespace: pvcNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      pvc.Spec.AccessModes,
			Resources:        pvc.Spec.Resources,
			VolumeName:       pvName, // Point to old PV
			StorageClassName: pvc.Spec.StorageClassName,
		},
	}
	_, err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Create(ctx, migratePVC, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create migrate PVC: %w", err)
	}

	// Create the original PVC pointing to the new PV
	originalPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: pvcNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      pvc.Spec.AccessModes,
			Resources:        pvc.Spec.Resources,
			VolumeName:       newPVName, // Point to new PV
			StorageClassName: newPVC.Spec.StorageClassName,
		},
	}
	_, err = clientset.CoreV1().PersistentVolumeClaims(pvcNamespace).Create(ctx, originalPVC, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create original PVC: %w", err)
	}

	// Step 7: Scale deployments and statefulsets back to original replicas
	fmt.Printf("Step 7: Scaling workloads back to original replicas...\n")

	for _, deployName := range deploymentsToScale {
		originalReplicas := deploymentReplicas[deployName]
		fmt.Printf("Scaling deployment %s back to %d replicas...\n", deployName, originalReplicas)
		deployment, err := clientset.AppsV1().Deployments(pvcNamespace).Get(ctx, deployName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get deployment %s: %w", deployName, err)
		}
		deployment.Spec.Replicas = &originalReplicas
		_, err = clientset.AppsV1().Deployments(pvcNamespace).Update(ctx, deployment, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to scale up deployment %s: %w", deployName, err)
		}
	}

	for _, stsName := range statefulsetsToScale {
		originalReplicas := statefulsetReplicas[stsName]
		fmt.Printf("Scaling statefulset %s back to %d replicas...\n", stsName, originalReplicas)
		statefulset, err := clientset.AppsV1().StatefulSets(pvcNamespace).Get(ctx, stsName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get statefulset %s: %w", stsName, err)
		}
		statefulset.Spec.Replicas = &originalReplicas
		_, err = clientset.AppsV1().StatefulSets(pvcNamespace).Update(ctx, statefulset, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to scale up statefulset %s: %w", stsName, err)
		}
	}

	fmt.Println("Migration completed successfully!")

	return nil
}

func main() {
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	storageClass := flag.String("storageclass", "", "(optional) filter by storage class name")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Println("Usage: migrate-pv <command> [options]")
		fmt.Println("Available commands: list, migrate")
		os.Exit(1)
	}

	command := flag.Arg(0)

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err)
	}

	switch command {
	case "list":
		pvs, err := listPersistentVolumes(clientset, *storageClass)
		if err != nil {
			panic(err)
		}

		if *storageClass != "" {
			fmt.Printf("Found %d PersistentVolumes with storage class '%s':\n", len(pvs), *storageClass)
		} else {
			fmt.Printf("Found %d PersistentVolumes:\n", len(pvs))
		}
		for _, pv := range pvs {
			fmt.Printf("- Name: %s, Capacity: %s, Status: %s\n",
				pv.Name,
				pv.Spec.Capacity.Storage().String(),
				pv.Status.Phase)
		}
	case "migrate":
		if flag.NArg() < 2 {
			fmt.Println("Usage: migrate-pv migrate <pv-name>")
			os.Exit(1)
		}
		pvName := flag.Arg(1)
		err := migratePersistentVolume(clientset, pvName)
		if err != nil {
			panic(err)
		}
	default:
		fmt.Printf("Unknown command: %s\n", command)
		fmt.Println("Available commands: list, migrate")
		os.Exit(1)
	}
}
