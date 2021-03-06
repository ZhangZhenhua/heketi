//
// Copyright (c) 2015 The heketi Authors
//
// This file is licensed to you under your choice of the GNU Lesser
// General Public License, version 3 or any later version (LGPLv3 or
// later), or the GNU General Public License, version 2 (GPLv2), in all
// cases as published by the Free Software Foundation.
//

package glusterfs

import (
	"fmt"

	"github.com/boltdb/bolt"
	"github.com/heketi/heketi/executors"
	wdb "github.com/heketi/heketi/pkg/db"
	"github.com/heketi/heketi/pkg/glusterfs/api"
	"github.com/heketi/heketi/pkg/utils"
)

func tryAllocateBrickOnDevice(v *VolumeEntry, device *DeviceEntry,
	setlist []*BrickEntry, brick_size uint64) *BrickEntry {

	// Do not allow a device from the same node to be in the set
	deviceOk := true
	for _, brickInSet := range setlist {
		if brickInSet.Info.NodeId == device.NodeId {
			deviceOk = false
		}
	}

	if !deviceOk {
		return nil
	}

	// Try to allocate a brick on this device
	brick := device.NewBrickEntry(brick_size,
		float64(v.Info.Snapshot.Factor),
		v.Info.Gid, v.Info.Id)

	return brick
}

func findDeviceAndBrickForSet(tx *bolt.Tx, v *VolumeEntry,
	devcache map[string](*DeviceEntry),
	deviceCh <-chan string,
	errc <-chan error,
	setlist []*BrickEntry,
	brick_size uint64) (*BrickEntry, *DeviceEntry, error) {

	// Check the ring for devices to place the brick
	for deviceId := range deviceCh {

		// Get device entry from cache if possible
		device, ok := devcache[deviceId]
		if !ok {
			// Get device entry from db otherwise
			var err error
			device, err = NewDeviceEntryFromId(tx, deviceId)
			if err != nil {
				return nil, nil, err
			}
			devcache[deviceId] = device
		}

		brick := tryAllocateBrickOnDevice(v, device, setlist, brick_size)
		if brick == nil {
			continue
		}

		return brick, device, nil
	}

	// Check if allocator returned an error
	if err := <-errc; err != nil {
		return nil, nil, err
	}

	// No devices found
	return nil, nil, ErrNoSpace
}

type BrickAllocation struct {
	Bricks  []*BrickEntry
	Devices []*DeviceEntry
}

func allocateBricks(
	db wdb.RODB,
	allocator Allocator,
	cluster string,
	v *VolumeEntry,
	bricksets int,
	brick_size uint64) (*BrickAllocation, error) {

	r := &BrickAllocation{
		Bricks:  []*BrickEntry{},
		Devices: []*DeviceEntry{},
	}

	devcache := map[string](*DeviceEntry){}

	err := db.View(func(tx *bolt.Tx) error {
		txdb := wdb.WrapTx(tx)

		// Determine allocation for each brick required for this volume
		for brick_num := 0; brick_num < bricksets; brick_num++ {
			logger.Info("brick_num: %v", brick_num)

			// Create a brick set list to later make sure that the
			// proposed bricks and devices are acceptable
			setlist := make([]*BrickEntry, 0)

			// Generate an id for the brick
			brickId := utils.GenUUID()

			// Get allocator generator
			// The same generator should be used for the brick and its replicas
			deviceCh, done, errc := allocator.GetNodes(txdb, cluster, brickId)
			defer func() {
				close(done)
			}()

			// Check location has space for each brick and its replicas
			for i := 0; i < v.Durability.BricksInSet(); i++ {
				logger.Debug("%v / %v", i, v.Durability.BricksInSet())

				brick, device, err := findDeviceAndBrickForSet(tx,
					v, devcache, deviceCh, errc, setlist,
					brick_size)
				if err != nil {
					return err
				}

				// If the first in the set, then reset the id
				if i == 0 {
					brick.SetId(brickId)
				}

				// Save the brick entry to create later
				r.Bricks = append(r.Bricks, brick)
				r.Devices = append(r.Devices, device)

				setlist = append(setlist, brick)

				device.BrickAdd(brick.Id())
			}
		}

		return nil
	})
	if err != nil {
		return r, err
	}

	// Only assign bricks to the volume object on success
	for _, brick := range r.Bricks {
		logger.Debug("Adding brick %v to volume %v", brick.Id(), v.Info.Id)
		v.BrickAdd(brick.Id())
	}

	return r, nil
}

func (v *VolumeEntry) allocBricksInCluster(db wdb.DB,
	allocator Allocator,
	cluster string,
	gbsize int) ([]*BrickEntry, error) {

	size := uint64(gbsize) * GB

	// Setup a brick size generator
	// Note: subsequent calls to gen need to return decreasing
	//       brick sizes in order for the following code to work!
	gen := v.Durability.BrickSizeGenerator(size)

	// Try decreasing possible brick sizes until space is found
	for {
		// Determine next possible brick size
		sets, brick_size, err := gen()
		if err != nil {
			logger.Err(err)
			return nil, err
		}

		num_bricks := sets * v.Durability.BricksInSet()

		logger.Debug("brick_size = %v", brick_size)
		logger.Debug("sets = %v", sets)
		logger.Debug("num_bricks = %v", num_bricks)

		// Check that the volume would not have too many bricks
		if (num_bricks + len(v.Bricks)) > BrickMaxNum {
			logger.Debug("Maximum number of bricks reached")
			return nil, ErrMaxBricks
		}

		// Allocate bricks in the cluster
		brick_entries, err := v.allocBricks(db, allocator, cluster, sets, brick_size)
		if err == ErrNoSpace {
			logger.Debug("No space, re-trying with smaller brick size")
			continue
		}
		if err != nil {
			logger.Err(err)
			return nil, err
		}

		// We were able to allocate bricks
		return brick_entries, nil
	}
}

func (v *VolumeEntry) getBrickEntryfromBrickName(db wdb.RODB, brickname string) (brickEntry *BrickEntry, e error) {

	var nodeEntry *NodeEntry
	for _, brickid := range v.BricksIds() {

		err := db.View(func(tx *bolt.Tx) error {
			var err error
			brickEntry, err = NewBrickEntryFromId(tx, brickid)
			if err != nil {
				return err
			}
			nodeEntry, err = NewNodeEntryFromId(tx, brickEntry.Info.NodeId)
			if err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return nil, err
		}

		if brickname == fmt.Sprintf("%v:%v", nodeEntry.Info.Hostnames.Storage[0], brickEntry.Info.Path) {
			return brickEntry, nil
		}
	}

	return nil, ErrNotFound
}

func (v *VolumeEntry) getBrickSetForBrickId(db wdb.DB,
	executor executors.Executor,
	oldBrickId string, node string) ([]*BrickEntry, error) {

	setlist := make([]*BrickEntry, 0)

	// Determine the setlist by getting data from Gluster
	vinfo, err := executor.VolumeInfo(node, v.Info.Name)
	if err != nil {
		logger.LogError("Unable to get volume info from gluster node %v for volume %v: %v", node, v.Info.Name, err)
		return setlist, err
	}

	var slicestartindex int
	var foundbrickset bool
	var brick executors.Brick
	// BrickList in volume info is a slice of all bricks in volume
	// We loop over the slice in steps of BricksInSet()
	// If brick to be replaced is found in an iteration, other bricks in that slice form the setlist
	for slicestartindex = 0; slicestartindex <= len(vinfo.Bricks.BrickList)-v.Durability.BricksInSet(); slicestartindex = slicestartindex + v.Durability.BricksInSet() {
		setlist = make([]*BrickEntry, 0)
		for _, brick = range vinfo.Bricks.BrickList[slicestartindex : slicestartindex+v.Durability.BricksInSet()] {
			brickentry, err := v.getBrickEntryfromBrickName(db, brick.Name)
			if err != nil {
				logger.LogError("Unable to create brick entry using brick name:%v, error: %v", brick.Name, err)
				return setlist, err
			}
			if brickentry.Id() == oldBrickId {
				foundbrickset = true
			} else {
				setlist = append(setlist, brickentry)
			}
		}
		if foundbrickset {
			break
		}
	}

	if !foundbrickset {
		logger.LogError("Unable to find brick set for brick %v, db is possibly corrupt", oldBrickId)
		return setlist, ErrNotFound
	}

	return setlist, nil
}

// canReplaceBrickInBrickSet
// check if a BrickSet is in a state where it's possible
// to replace a given one of its bricks:
// - no heals going on on the brick to be replaced
// - enough bricks of the set are up
func (v *VolumeEntry) canReplaceBrickInBrickSet(db wdb.DB,
	executor executors.Executor,
	brick *BrickEntry,
	node string,
	setlist []*BrickEntry) error {

	// Get self heal status for this brick's volume
	healinfo, err := executor.HealInfo(node, v.Info.Name)
	if err != nil {
		return err
	}

	var onlinePeerBrickCount = 0
	for _, brickHealStatus := range healinfo.Bricks.BrickList {
		// Gluster has a bug that it does not send Name for bricks that are down.
		// Skip such bricks; it is safe because it is not source if it is down
		if brickHealStatus.Name == "information not available" {
			continue
		}
		iBrickEntry, err := v.getBrickEntryfromBrickName(db, brickHealStatus.Name)
		if err != nil {
			return fmt.Errorf("Unable to determine heal status of brick")
		}
		if iBrickEntry.Id() == brick.Id() {
			// If we are here, it means the brick to be replaced is
			// up and running. We need to ensure that it is not a
			// source for any files.
			if brickHealStatus.NumberOfEntries != "-" &&
				brickHealStatus.NumberOfEntries != "0" {
				return fmt.Errorf("Cannot replace brick %v as it is source brick for data to be healed", iBrickEntry.Id())
			}
		}
		for _, brickInSet := range setlist {
			if brickInSet.Id() == iBrickEntry.Id() {
				onlinePeerBrickCount++
			}
		}
	}
	if onlinePeerBrickCount < v.Durability.QuorumBrickCount() {
		return fmt.Errorf("Cannot replace brick %v as only %v of %v "+
			"required peer bricks are online",
			brick.Id(), onlinePeerBrickCount,
			v.Durability.QuorumBrickCount())
	}

	return nil
}

func (v *VolumeEntry) replaceBrickInVolume(db wdb.DB, executor executors.Executor,
	allocator Allocator,
	oldBrickId string) (e error) {

	var oldBrickEntry *BrickEntry
	var oldDeviceEntry *DeviceEntry
	var newDeviceEntry *DeviceEntry
	var oldBrickNodeEntry *NodeEntry
	var newBrickNodeEntry *NodeEntry
	var newBrickEntry *BrickEntry

	if api.DurabilityDistributeOnly == v.Info.Durability.Type {
		return fmt.Errorf("replace brick is not supported for volume durability type %v", v.Info.Durability.Type)
	}

	err := db.View(func(tx *bolt.Tx) error {
		var err error
		oldBrickEntry, err = NewBrickEntryFromId(tx, oldBrickId)
		if err != nil {
			return err
		}

		oldDeviceEntry, err = NewDeviceEntryFromId(tx, oldBrickEntry.Info.DeviceId)
		if err != nil {
			return err
		}
		oldBrickNodeEntry, err = NewNodeEntryFromId(tx, oldBrickEntry.Info.NodeId)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	node := oldBrickNodeEntry.ManageHostName()
	err = executor.GlusterdCheck(node)
	if err != nil {
		node, err = GetVerifiedManageHostname(db, executor, oldBrickNodeEntry.Info.ClusterId)
		if err != nil {
			return err
		}
	}

	setlist, err := v.getBrickSetForBrickId(db, executor, oldBrickId, node)
	if err != nil {
		return err
	}

	err = v.canReplaceBrickInBrickSet(db, executor, oldBrickEntry, node, setlist)
	if err != nil {
		return err
	}

	//Create an Id for new brick
	newBrickId := utils.GenUUID()

	// Check the ring for devices to place the brick
	deviceCh, done, errc := allocator.GetNodes(db, v.Info.Cluster, newBrickId)
	defer func() {
		close(done)
	}()

	for deviceId := range deviceCh {

		// Get device entry
		err = db.View(func(tx *bolt.Tx) error {
			newDeviceEntry, err = NewDeviceEntryFromId(tx, deviceId)
			if err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}

		// Skip same device
		if oldDeviceEntry.Info.Id == newDeviceEntry.Info.Id {
			continue
		}

		// Do not allow a device from the same node to be
		// in the set
		deviceOk := true
		for _, brickInSet := range setlist {
			if brickInSet.Info.NodeId == newDeviceEntry.NodeId {
				deviceOk = false
			}
		}

		if !deviceOk {
			continue
		}

		// Try to allocate a brick on this device
		// NewBrickEntry would deduct storage from device entry
		// which we will save to disk, hence reload the latest device
		// entry to get latest storage state of device
		err = db.Update(func(tx *bolt.Tx) error {
			newDeviceEntry, err := NewDeviceEntryFromId(tx, deviceId)
			if err != nil {
				return err
			}
			newBrickEntry = newDeviceEntry.NewBrickEntry(oldBrickEntry.Info.Size,
				float64(v.Info.Snapshot.Factor),
				v.Info.Gid, v.Info.Id)
			err = newDeviceEntry.Save(tx)
			if err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}

		// Determine if it was successful
		if newBrickEntry == nil {
			continue
		}

		defer func() {
			if e != nil {
				db.Update(func(tx *bolt.Tx) error {
					newDeviceEntry, err = NewDeviceEntryFromId(tx, newBrickEntry.Info.DeviceId)
					if err != nil {
						return err
					}
					newDeviceEntry.StorageFree(newBrickEntry.TotalSize())
					newDeviceEntry.Save(tx)
					return nil
				})
			}
		}()

		err = db.View(func(tx *bolt.Tx) error {
			newBrickNodeEntry, err = NewNodeEntryFromId(tx, newBrickEntry.Info.NodeId)
			if err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}

		newBrickEntry.SetId(newBrickId)
		var brickEntries []*BrickEntry
		brickEntries = append(brickEntries, newBrickEntry)
		err = CreateBricks(db, executor, brickEntries)
		if err != nil {
			return err
		}

		defer func() {
			if e != nil {
				DestroyBricks(db, executor, brickEntries)
			}
		}()

		var oldBrick executors.BrickInfo
		var newBrick executors.BrickInfo

		oldBrick.Path = oldBrickEntry.Info.Path
		oldBrick.Host = oldBrickNodeEntry.StorageHostName()
		newBrick.Path = newBrickEntry.Info.Path
		newBrick.Host = newBrickNodeEntry.StorageHostName()

		err = executor.VolumeReplaceBrick(node, v.Info.Name, &oldBrick, &newBrick)
		if err != nil {
			return err
		}

		// After this point we should not call any defer func()
		// We don't have a *revert* of replace brick operation

		_ = oldBrickEntry.Destroy(db, executor)

		// We must read entries from db again as state on disk might
		// have changed

		err = db.Update(func(tx *bolt.Tx) error {
			err = newBrickEntry.Save(tx)
			if err != nil {
				return err
			}
			reReadNewDeviceEntry, err := NewDeviceEntryFromId(tx, newBrickEntry.Info.DeviceId)
			if err != nil {
				return err
			}
			reReadNewDeviceEntry.BrickAdd(newBrickEntry.Id())
			err = reReadNewDeviceEntry.Save(tx)
			if err != nil {
				return err
			}

			reReadVolEntry, err := NewVolumeEntryFromId(tx, newBrickEntry.Info.VolumeId)
			if err != nil {
				return err
			}
			reReadVolEntry.BrickAdd(newBrickEntry.Id())
			err = reReadVolEntry.removeBrickFromDb(tx, oldBrickEntry)
			if err != nil {
				return err
			}
			err = reReadVolEntry.Save(tx)
			if err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			logger.Err(err)
		}

		logger.Info("replaced brick:%v on node:%v at path:%v with brick:%v on node:%v at path:%v",
			oldBrickEntry.Id(), oldBrickEntry.Info.NodeId, oldBrickEntry.Info.Path,
			newBrickEntry.Id(), newBrickEntry.Info.NodeId, newBrickEntry.Info.Path)

		return nil
	}
	// Check if allocator returned an error
	if err := <-errc; err != nil {
		return err
	}

	// No device found
	return ErrNoReplacement
}

func (v *VolumeEntry) allocBricks(
	db wdb.DB,
	allocator Allocator,
	cluster string,
	bricksets int,
	brick_size uint64) (brick_entries []*BrickEntry, e error) {

	// Setup garbage collector function in case of error
	defer func() {

		// Check the named return value 'err'
		if e != nil {
			logger.Debug("Error detected.  Cleaning up volume %v: Len(%v) ", v.Info.Id, len(brick_entries))
			db.Update(func(tx *bolt.Tx) error {
				for _, brick := range brick_entries {
					v.removeBrickFromDb(tx, brick)
				}
				return nil
			})
		}
	}()

	// mimic the previous unconditional db update behavior
	err := db.Update(func(tx *bolt.Tx) error {
		wtx := wdb.WrapTx(tx)
		r, e := allocateBricks(wtx, allocator, cluster, v, bricksets, brick_size)
		if e != nil {
			return e
		}
		for _, x := range r.Bricks {
			err := x.Save(tx)
			if err != nil {
				return err
			}
		}
		for _, x := range r.Devices {
			err := x.Save(tx)
			if err != nil {
				return err
			}
		}
		brick_entries = r.Bricks
		return nil
	})
	if err != nil {
		return brick_entries, err
	}

	return brick_entries, nil
}

func (v *VolumeEntry) removeBrickFromDb(tx *bolt.Tx, brick *BrickEntry) error {

	// Access device
	device, err := NewDeviceEntryFromId(tx, brick.Info.DeviceId)
	if err != nil {
		logger.Err(err)
		return err
	}

	// Deallocate space on device
	device.StorageFree(brick.TotalSize())

	// Delete brick from device
	device.BrickDelete(brick.Info.Id)

	// Save device
	err = device.Save(tx)
	if err != nil {
		logger.Err(err)
		return err
	}

	// Delete brick entryfrom db
	err = brick.Delete(tx)
	if err != nil {
		logger.Err(err)
		return err
	}

	// Delete brick from volume db
	v.BrickDelete(brick.Info.Id)
	if err != nil {
		logger.Err(err)
		return err
	}

	return nil
}
