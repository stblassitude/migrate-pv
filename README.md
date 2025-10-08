# Kubernetes PersistentVolume Migration Tool

A Go-based tool for migrating Kubernetes PersistentVolumes between storage classes with zero data loss.

## WARNING

**THIS CODE IS BARELY TESTED. USE AT YOUR OWN RISK. IT WILL EAT YOUR DATA.**

In particular, there is no real error handling, and no recovery mechanism. Make sure you have proper backups before running this tool.

The code has been created with the help of a GenAI coding assistant, and is likely wrong.

## How It Works

The `migratePersistentVolume` function performs a safe, automated migration of a PersistentVolume to a new storage class by following these steps:

### Migration Process

**Step 1: Discovery**
- Locates the specified PersistentVolume (PV) and its bound PersistentVolumeClaim (PVC)
- Identifies all Deployments and StatefulSets using the PVC
- Records original replica counts for later restoration

**Step 2: Workload Identification**
- Scans Deployments for volumes mounting the target PVC
- Scans StatefulSets for both:
  - Regular volumes mounting the target PVC
  - VolumeClaimTemplates that generate the target PVC (matches pattern: `<template-name>-<statefulset-name>-<ordinal>`)

**Step 3: Scale Down**
- Scales all identified Deployments to 0 replicas
- Scales all identified StatefulSets to 0 replicas
- Waits for all pods to terminate gracefully

**Step 4: Create Migration Target**
- Creates a new PVC with the `-migrate` suffix using the default storage class
- Asynchronously monitors and updates the new PV's reclaim policy to `Retain` once provisioned
- Ensures data will be preserved during the swap operation

**Step 5: Data Migration**
- Creates a temporary migration pod with both PVCs mounted
- Uses `busybox` to copy all data from the old PVC to the new PVC (`cp -ar /old-data/. /new-data/`)
- Waits for the migration to complete successfully
- Cleans up the migration pod after completion

**Step 6: PVC/PV Swap**
- Removes finalizers from both PVCs
- Deletes both PVCs (volumes are retained due to `Retain` policy)
- Clears `claimRef` from both PVs to make them available for rebinding
- Recreates the PVCs with swapped PV bindings:
  - Original PVC name now points to the new PV (with migrated data)
  - `-migrate` PVC now points to the old PV (for backup/rollback)

**Step 7: Scale Up**
- Restores all Deployments to their original replica counts
- Restores all StatefulSets to their original replica counts
- Workloads resume with the new storage class

## Usage

### List PersistentVolumes
```bash
./migrate-pv list [--storageclass <storage-class-name>]
```

### Migrate a PersistentVolume
```bash
./migrate-pv migrate <pv-name>
```

## Features
- **Automatic workload management**: Discovers and manages Deployments and StatefulSets
- **StatefulSet support**: Handles both regular volumes and volumeClaimTemplates
- **Safety mechanisms**: Uses `Retain` reclaim policy to prevent accidental data loss
- **Backup preservation**: Keeps the old PV available in the PVC for rollback `-migrate`
- **Automated restoration**: Scales workloads back to their original state after migration

## Requirements
- Go 1.25+
- Kubernetes cluster access (kubeconfig)
- Sufficient permissions to:
    - Read/write PVs and PVCs
    - Read/write Deployments and StatefulSets
    - Create/delete Pods

## Safety Considerations
- The migration process scales down workloads, causing temporary unavailability
- The old PV is preserved in the PVC for rollback purposes `-migrate`
- Both PVs have `Retain` reclaim policy to prevent accidental deletion
- Migration time depends on the volume size and cluster I/O performance
