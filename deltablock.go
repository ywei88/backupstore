package backupstore

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"

	. "github.com/longhorn/backupstore/logging"
	"github.com/longhorn/backupstore/util"
)

type DeltaBackupConfig struct {
	Volume   *Volume
	Snapshot *Snapshot
	DestURL  string
	DeltaOps DeltaBlockBackupOperations
	Labels   map[string]string
}

type BlockMapping struct {
	Offset        int64
	BlockChecksum string
}

type DeltaBlockBackupOperations interface {
	HasSnapshot(id, volumeID string) bool
	CompareSnapshot(id, compareID, volumeID string) (*Mappings, error)
	OpenSnapshot(id, volumeID string) error
	ReadSnapshot(id, volumeID string, start int64, data []byte) error
	CloseSnapshot(id, volumeID string) error
	UpdateBackupStatus(id, volumeID string, backupProgress int, backupURL string, err string) error
}

const (
	DEFAULT_BLOCK_SIZE = 2097152

	BLOCKS_DIRECTORY      = "blocks"
	BLOCK_SEPARATE_LAYER1 = 2
	BLOCK_SEPARATE_LAYER2 = 4

	PROGRESS_PERCENTAGE_BACKUP_SNAPSHOT = 95
	PROGRESS_PERCENTAGE_BACKUP_TOTAL    = 100
)

func CreateDeltaBlockBackup(config *DeltaBackupConfig) (string, error) {
	if config == nil {
		return "", fmt.Errorf("Invalid empty config for backup")
	}

	volume := config.Volume
	snapshot := config.Snapshot
	destURL := config.DestURL
	deltaOps := config.DeltaOps
	if deltaOps == nil {
		return "", fmt.Errorf("Missing DeltaBlockBackupOperations")
	}

	bsDriver, err := GetBackupStoreDriver(destURL)
	if err != nil {
		return "", err
	}

	if err := addVolume(volume, bsDriver); err != nil {
		return "", err
	}

	// Update volume from backupstore
	volume, err = loadVolume(volume.Name, bsDriver)
	if err != nil {
		return "", err
	}

	lastBackupName := volume.LastBackupName

	if err := deltaOps.OpenSnapshot(snapshot.Name, volume.Name); err != nil {
		return "", err
	}

	var lastSnapshotName string
	var lastBackup *Backup
	if lastBackupName != "" {
		lastBackup, err = loadBackup(lastBackupName, volume.Name, bsDriver)
		if err != nil {
			deltaOps.CloseSnapshot(snapshot.Name, volume.Name)
			return "", err
		}

		lastSnapshotName = lastBackup.SnapshotName
		if lastSnapshotName == snapshot.Name {
			//Generate full snapshot if the snapshot has been backed up last time
			lastSnapshotName = ""
			log.Debug("Would create full snapshot metadata")
		} else if !deltaOps.HasSnapshot(lastSnapshotName, volume.Name) {
			// It's possible that the snapshot in backupstore doesn't exist
			// in local storage
			lastSnapshotName = ""
			log.WithFields(logrus.Fields{
				LogFieldReason:   LogReasonFallback,
				LogFieldObject:   LogObjectSnapshot,
				LogFieldSnapshot: lastSnapshotName,
				LogFieldVolume:   volume.Name,
			}).Debug("Cannot find last snapshot in local storage, would process with full backup")
		}
	}

	log.WithFields(logrus.Fields{
		LogFieldReason:       LogReasonStart,
		LogFieldObject:       LogObjectSnapshot,
		LogFieldEvent:        LogEventCompare,
		LogFieldSnapshot:     snapshot.Name,
		LogFieldLastSnapshot: lastSnapshotName,
	}).Debug("Generating snapshot changed blocks metadata")

	delta, err := deltaOps.CompareSnapshot(snapshot.Name, lastSnapshotName, volume.Name)
	if err != nil {
		deltaOps.CloseSnapshot(snapshot.Name, volume.Name)
		return "", err
	}
	if delta.BlockSize != DEFAULT_BLOCK_SIZE {
		deltaOps.CloseSnapshot(snapshot.Name, volume.Name)
		return "", fmt.Errorf("currently doesn't support different block sizes driver other than %v", DEFAULT_BLOCK_SIZE)
	}
	log.WithFields(logrus.Fields{
		LogFieldReason:       LogReasonComplete,
		LogFieldObject:       LogObjectSnapshot,
		LogFieldEvent:        LogEventCompare,
		LogFieldSnapshot:     snapshot.Name,
		LogFieldLastSnapshot: lastSnapshotName,
	}).Debug("Generated snapshot changed blocks metadata")

	log.WithFields(logrus.Fields{
		LogFieldReason:   LogReasonStart,
		LogFieldEvent:    LogEventBackup,
		LogFieldObject:   LogObjectSnapshot,
		LogFieldSnapshot: snapshot.Name,
	}).Debug("Creating backup")

	deltaBackup := &Backup{
		Name:         util.GenerateName("backup"),
		VolumeName:   volume.Name,
		SnapshotName: snapshot.Name,
		Blocks:       []BlockMapping{},
	}

	go func() {
		defer deltaOps.CloseSnapshot(snapshot.Name, volume.Name)
		if progress, backup, err := performIncrementalBackup(config, delta, deltaBackup, lastBackup, bsDriver); err != nil {
			deltaOps.UpdateBackupStatus(snapshot.Name, volume.Name, progress, "", err.Error())
		} else {
			deltaOps.UpdateBackupStatus(snapshot.Name, volume.Name, progress, backup, "")
		}
	}()
	return deltaBackup.Name, nil
}

func performIncrementalBackup(config *DeltaBackupConfig, delta *Mappings, deltaBackup *Backup, lastBackup *Backup,
	bsDriver BackupStoreDriver) (int, string, error) {

	volume := config.Volume
	snapshot := config.Snapshot
	destURL := config.DestURL
	deltaOps := config.DeltaOps

	var progress int
	mCounts := len(delta.Mappings)
	newBlocks := int64(0)
	for m, d := range delta.Mappings {
		if d.Size%delta.BlockSize != 0 {
			return progress, "", fmt.Errorf("Mapping's size %v is not multiples of backup block size %v",
				d.Size, delta.BlockSize)
		}
		block := make([]byte, DEFAULT_BLOCK_SIZE)
		blkCounts := d.Size / delta.BlockSize
		for i := int64(0); i < blkCounts; i++ {
			offset := d.Offset + i*delta.BlockSize
			log.Debugf("Backup for %v: segment %v/%v, blocks %v/%v", snapshot.Name, m+1, mCounts, i+1, blkCounts)
			err := deltaOps.ReadSnapshot(snapshot.Name, volume.Name, offset, block)
			if err != nil {
				return progress, "", err
			}
			checksum := util.GetChecksum(block)
			blkFile := getBlockFilePath(volume.Name, checksum)
			if bsDriver.FileSize(blkFile) >= 0 {
				blockMapping := BlockMapping{
					Offset:        offset,
					BlockChecksum: checksum,
				}
				deltaBackup.Blocks = append(deltaBackup.Blocks, blockMapping)
				log.Debugf("Found existed block match at %v", blkFile)
				continue
			}

			rs, err := util.CompressData(block)
			if err != nil {
				return progress, "", err
			}

			if err := bsDriver.Write(blkFile, rs); err != nil {
				return progress, "", err
			}
			log.Debugf("Created new block file at %v", blkFile)

			newBlocks++
			blockMapping := BlockMapping{
				Offset:        offset,
				BlockChecksum: checksum,
			}
			deltaBackup.Blocks = append(deltaBackup.Blocks, blockMapping)
		}
		progress = int((float64(m+1) / float64(mCounts)) * PROGRESS_PERCENTAGE_BACKUP_SNAPSHOT)
		deltaOps.UpdateBackupStatus(snapshot.Name, volume.Name, progress, "", "")
	}

	log.WithFields(logrus.Fields{
		LogFieldReason:   LogReasonComplete,
		LogFieldEvent:    LogEventBackup,
		LogFieldObject:   LogObjectSnapshot,
		LogFieldSnapshot: snapshot.Name,
	}).Debug("Created snapshot changed blocks")

	backup := mergeSnapshotMap(deltaBackup, lastBackup)
	backup.SnapshotName = snapshot.Name
	backup.SnapshotCreatedAt = snapshot.CreatedTime
	backup.CreatedTime = util.Now()
	backup.Size = int64(len(backup.Blocks)) * DEFAULT_BLOCK_SIZE
	backup.Labels = config.Labels

	if err := saveBackup(backup, bsDriver); err != nil {
		return progress, "", err
	}

	volume, err := loadVolume(volume.Name, bsDriver)
	if err != nil {
		return progress, "", err
	}

	volume.LastBackupName = backup.Name
	volume.LastBackupAt = backup.SnapshotCreatedAt
	volume.BlockCount = volume.BlockCount + newBlocks

	if err := saveVolume(volume, bsDriver); err != nil {
		return progress, "", err
	}

	return PROGRESS_PERCENTAGE_BACKUP_TOTAL, encodeBackupURL(backup.Name, volume.Name, destURL), nil
}

func mergeSnapshotMap(deltaBackup, lastBackup *Backup) *Backup {
	if lastBackup == nil {
		return deltaBackup
	}
	backup := &Backup{
		Name:         deltaBackup.Name,
		VolumeName:   deltaBackup.VolumeName,
		SnapshotName: deltaBackup.SnapshotName,
		Blocks:       []BlockMapping{},
	}
	var d, l int
	for d, l = 0, 0; d < len(deltaBackup.Blocks) && l < len(lastBackup.Blocks); {
		dB := deltaBackup.Blocks[d]
		lB := lastBackup.Blocks[l]
		if dB.Offset == lB.Offset {
			backup.Blocks = append(backup.Blocks, dB)
			d++
			l++
		} else if dB.Offset < lB.Offset {
			backup.Blocks = append(backup.Blocks, dB)
			d++
		} else {
			//dB.Offset > lB.offset
			backup.Blocks = append(backup.Blocks, lB)
			l++
		}
	}

	if d == len(deltaBackup.Blocks) {
		backup.Blocks = append(backup.Blocks, lastBackup.Blocks[l:]...)
	} else {
		backup.Blocks = append(backup.Blocks, deltaBackup.Blocks[d:]...)
	}

	return backup
}

func RestoreDeltaBlockBackup(backupURL, volDevName string) error {
	bsDriver, err := GetBackupStoreDriver(backupURL)
	if err != nil {
		return err
	}

	srcBackupName, srcVolumeName, err := decodeBackupURL(backupURL)
	if err != nil {
		return err
	}

	vol, err := loadVolume(srcVolumeName, bsDriver)
	if err != nil {
		return generateError(logrus.Fields{
			LogFieldVolume:    srcVolumeName,
			LogEventBackupURL: backupURL,
		}, "Volume doesn't exist in backupstore: %v", err)
	}

	if vol.Size == 0 || vol.Size%DEFAULT_BLOCK_SIZE != 0 {
		return fmt.Errorf("Read invalid volume size %v", vol.Size)
	}

	volDev, err := os.Create(volDevName)
	if err != nil {
		return err
	}
	defer volDev.Close()

	stat, err := volDev.Stat()
	if err != nil {
		return err
	}

	backup, err := loadBackup(srcBackupName, srcVolumeName, bsDriver)
	if err != nil {
		return err
	}

	log.WithFields(logrus.Fields{
		LogFieldReason:     LogReasonStart,
		LogFieldEvent:      LogEventRestore,
		LogFieldObject:     LogFieldSnapshot,
		LogFieldSnapshot:   srcBackupName,
		LogFieldOrigVolume: srcVolumeName,
		LogFieldVolumeDev:  volDevName,
		LogEventBackupURL:  backupURL,
	}).Debug()
	blkCounts := len(backup.Blocks)
	for i, block := range backup.Blocks {
		log.Debugf("Restore for %v: block %v, %v/%v", volDevName, block.BlockChecksum, i+1, blkCounts)
		if err := restoreBlockToFile(srcVolumeName, volDev, bsDriver, block); err != nil {
			return err
		}
	}

	// We want to truncate regular files, but not device
	if stat.Mode()&os.ModeType == 0 {
		log.Debugf("Truncate %v to size %v", volDevName, vol.Size)
		if err := volDev.Truncate(vol.Size); err != nil {
			return err
		}
	}

	return nil
}

func restoreBlockToFile(volumeName string, volDev *os.File, bsDriver BackupStoreDriver, blk BlockMapping) error {
	blkFile := getBlockFilePath(volumeName, blk.BlockChecksum)
	rc, err := bsDriver.Read(blkFile)
	if err != nil {
		return err
	}
	defer rc.Close()
	r, err := util.DecompressAndVerify(rc, blk.BlockChecksum)
	if err != nil {
		return err
	}
	if _, err := volDev.Seek(blk.Offset, 0); err != nil {
		return err
	}
	if _, err := io.CopyN(volDev, r, DEFAULT_BLOCK_SIZE); err != nil {
		return err
	}
	return nil
}

func RestoreDeltaBlockBackupIncrementally(backupURL, volDevName, lastBackupName string) error {
	bsDriver, err := GetBackupStoreDriver(backupURL)
	if err != nil {
		return err
	}

	srcBackupName, srcVolumeName, err := decodeBackupURL(backupURL)
	if err != nil {
		return err
	}

	vol, err := loadVolume(srcVolumeName, bsDriver)
	if err != nil {
		return generateError(logrus.Fields{
			LogFieldVolume:    srcVolumeName,
			LogEventBackupURL: backupURL,
		}, "Volume doesn't exist in backupstore: %v", err)
	}

	if vol.Size == 0 || vol.Size%DEFAULT_BLOCK_SIZE != 0 {
		return fmt.Errorf("Read invalid volume size %v", vol.Size)
	}

	// check lastBackupName
	if !util.ValidateName(lastBackupName) {
		return fmt.Errorf("Invalid parameter lastBackupName %v", lastBackupName)
	}

	// check volDev
	var volDev *os.File
	if _, err := os.Stat(volDevName); os.IsNotExist(err) {
		volDev, err = os.Create(volDevName)
		if err != nil {
			return err
		}
		logrus.Debugf("Created new file %v", volDevName)
	} else {
		volDev, err = os.OpenFile(volDevName, os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		logrus.Debugf("File %v existed\n", volDevName)
	}
	defer volDev.Close()

	stat, err := volDev.Stat()
	if err != nil {
		return err
	}

	lastBackup, err := loadBackup(lastBackupName, srcVolumeName, bsDriver)
	if err != nil {
		return err
	}
	backup, err := loadBackup(srcBackupName, srcVolumeName, bsDriver)
	if err != nil {
		return err
	}

	log.WithFields(logrus.Fields{
		LogFieldReason:     LogReasonStart,
		LogFieldEvent:      LogEventRestoreIncre,
		LogFieldObject:     LogFieldSnapshot,
		LogFieldSnapshot:   srcBackupName,
		LogFieldOrigVolume: srcVolumeName,
		LogFieldVolumeDev:  volDevName,
		LogEventBackupURL:  backupURL,
	}).Debugf("Started incrementally restoring from %v to %v", lastBackup, backup)

	emptyBlock := make([]byte, DEFAULT_BLOCK_SIZE)
	for b, l := 0, 0; b < len(backup.Blocks) || l < len(lastBackup.Blocks); {
		if b >= len(backup.Blocks) {
			if err := fillBlockToFile(&emptyBlock, volDev, lastBackup.Blocks[l].Offset); err != nil {
				return err
			}
			l++
			continue
		}
		if l >= len(lastBackup.Blocks) {
			if err := restoreBlockToFile(srcVolumeName, volDev, bsDriver, backup.Blocks[b]); err != nil {
				return err
			}
			b++
			continue
		}

		bB := backup.Blocks[b]
		lB := lastBackup.Blocks[l]
		if bB.Offset == lB.Offset {
			if bB.BlockChecksum != lB.BlockChecksum {
				if err := restoreBlockToFile(srcVolumeName, volDev, bsDriver, bB); err != nil {
					return err
				}
			}
			b++
			l++
		} else if bB.Offset < lB.Offset {
			if err := restoreBlockToFile(srcVolumeName, volDev, bsDriver, bB); err != nil {
				return err
			}
			b++
		} else {
			if err := fillBlockToFile(&emptyBlock, volDev, lB.Offset); err != nil {
				return err
			}
			l++
		}
	}

	// We want to truncate regular files, but not device
	if stat.Mode()&os.ModeType == 0 {
		log.Debugf("Truncate %v to size %v", volDevName, vol.Size)
		if err := volDev.Truncate(vol.Size); err != nil {
			return err
		}
	}

	return nil
}

func fillBlockToFile(block *[]byte, volDev *os.File, offset int64) error {
	if _, err := volDev.WriteAt(*block, offset); err != nil {
		return err
	}
	return nil
}

func DeleteBackupVolume(volumeName string, destURL string) error {
	bsDriver, err := GetBackupStoreDriver(destURL)
	if err != nil {
		return err
	}

	if err := removeVolume(volumeName, bsDriver); err != nil {
		return err
	}

	return nil
}

func DeleteDeltaBlockBackup(backupURL string) error {
	bsDriver, err := GetBackupStoreDriver(backupURL)
	if err != nil {
		return err
	}

	backupName, volumeName, err := decodeBackupURL(backupURL)
	if err != nil {
		return err
	}

	v, err := loadVolume(volumeName, bsDriver)
	if err != nil {
		return fmt.Errorf("Cannot find volume %v in backupstore", volumeName, err)
	}

	backup, err := loadBackup(backupName, volumeName, bsDriver)
	if err != nil {
		return err
	}
	discardBlockSet := make(map[string]bool)
	for _, blk := range backup.Blocks {
		discardBlockSet[blk.BlockChecksum] = true
	}
	discardBlockCounts := len(discardBlockSet)

	if err := removeBackup(backup, bsDriver); err != nil {
		return err
	}

	if backup.Name == v.LastBackupName {
		v.LastBackupName = ""
		v.LastBackupAt = ""
		if err := saveVolume(v, bsDriver); err != nil {
			return err
		}
	}

	backupNames, err := getBackupNamesForVolume(volumeName, bsDriver)
	if err != nil {
		return err
	}
	if len(backupNames) == 0 {
		log.Errorf("No snapshot existed for the volume %v, removing volume", volumeName)
		if err := removeVolume(volumeName, bsDriver); err != nil {
			log.Errorf("Failed to remove volume %v due to: %v", volumeName, err.Error())
		}
		return nil
	}

	log.Errorf("GC started")
	for _, backupName := range backupNames {
		backup, err := loadBackup(backupName, volumeName, bsDriver)
		if err != nil {
			return err
		}
		for _, blk := range backup.Blocks {
			if _, exists := discardBlockSet[blk.BlockChecksum]; exists {
				delete(discardBlockSet, blk.BlockChecksum)
				discardBlockCounts--
				if discardBlockCounts == 0 {
					break
				}
			}
		}
		if discardBlockCounts == 0 {
			break
		}
	}

	var blkFileList []string
	for blk := range discardBlockSet {
		blkFileList = append(blkFileList, getBlockFilePath(volumeName, blk))
		log.Errorf("Found unused blocks %v for volume %v", blk, volumeName)
	}
	if err := bsDriver.Remove(blkFileList...); err != nil {
		return err
	}
	log.Errorf("Removed unused blocks for volume ", volumeName)

	log.Errorf("GC completed")
	log.Errorf("Removed backupstore backup ", backupName)

	v, err = loadVolume(volumeName, bsDriver)
	if err != nil {
		return err
	}

	v.BlockCount -= int64(len(discardBlockSet))

	if err := saveVolume(v, bsDriver); err != nil {
		return err
	}

	return nil
}

func getBlockPath(volumeName string) string {
	return filepath.Join(getVolumePath(volumeName), BLOCKS_DIRECTORY) + "/"
}

func getBlockFilePath(volumeName, checksum string) string {
	blockSubDirLayer1 := checksum[0:BLOCK_SEPARATE_LAYER1]
	blockSubDirLayer2 := checksum[BLOCK_SEPARATE_LAYER1:BLOCK_SEPARATE_LAYER2]
	path := filepath.Join(getBlockPath(volumeName), blockSubDirLayer1, blockSubDirLayer2)
	fileName := checksum + ".blk"

	return filepath.Join(path, fileName)
}
