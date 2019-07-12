package cmd

import (
	"fmt"
	"github.com/urfave/cli"

	"github.com/longhorn/backupstore"
	"github.com/longhorn/backupstore/util"
)

func BackupRemoveCmd() cli.Command {
	return cli.Command{
		Name:    "remove",
		Aliases: []string{"rm", "delete"},
		Usage:   "remove a backup in objectstore: rm <backup>",
		Action:  cmdBackupRemove,
	}
}

func cmdBackupRemove(c *cli.Context) {
	if err := doBackupRemove(c); err != nil {
		panic(err)
	}
}

func doBackupRemove(c *cli.Context) error {
	if c.NArg() == 0 {
		return RequiredMissingError("backup URL")
	}
	backupURL := c.Args()[0]
	if backupURL == "" {
		return RequiredMissingError("backup URL")
	}
	backupURL = util.UnescapeURL(backupURL)

	if err := backupstore.DeleteDeltaBlockBackup(backupURL); err != nil {
		return err
	}
	return nil
}

func BackupVolumeRemoveCmd() cli.Command {
	return cli.Command{
		Name:    "removebackupvolume",
		Aliases: []string{"rmbv"},
		Usage:   "remove a backup volume in objectstore: rmbv <backupvolume>",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "volume",
				Usage: "volume name",
			},
		},
		Action: cmdBackupVolumeRemove,
	}
}

func cmdBackupVolumeRemove(c *cli.Context) {
	if err := doBackupVolumeRemove(c); err != nil {
		panic(err)
	}
}

func doBackupVolumeRemove(c *cli.Context) error {
	if c.NArg() == 0 {
		return RequiredMissingError("dest URL")
	}
	destURL := c.Args()[0]
	if destURL == "" {
		return RequiredMissingError("dest URL")
	}
	volumeName := c.String("volume")
	if volumeName != "" && !util.ValidateName(volumeName) {
		return fmt.Errorf("Invalid volume name %v for backup", volumeName)
	}

	if err := backupstore.DeleteBackupVolume(volumeName, destURL); err != nil {
		return err
	}
	return nil
}
