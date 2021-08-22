package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/joho/godotenv"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/walle/targz"
	"golang.org/x/crypto/openpgp"
)

func main() {
	unlock, err := lock()
	if err != nil {
		panic(err)
	}
	defer unlock()

	s := &script{}

	must(s.init)()
	fmt.Println("Successfully initialized resources.")
	err = s.stopContainersAndRun(s.takeBackup)
	if err != nil {
		panic(err)
	}
	fmt.Println("Successfully took backup.")
	must(s.encryptBackup)()
	fmt.Println("Successfully encrypted backup.")
	must(s.copyBackup)()
	fmt.Println("Successfully copied backup.")
	must(s.cleanBackup)()
	fmt.Println("Successfully cleaned local backup.")
	must(s.pruneOldBackups)()
	fmt.Println("Successfully pruned old backups.")
}

type script struct {
	ctx    context.Context
	cli    *client.Client
	mc     *minio.Client
	file   string
	bucket string
}

// lock opens a lock file without releasing it and returns a function that
// can be called once the lock shall be released again.
func lock() (func() error, error) {
	lockfile := "/var/dockervolumebackup.lock"
	lf, err := os.OpenFile(lockfile, os.O_CREATE, os.ModeAppend)
	if err != nil {
		return nil, fmt.Errorf("lock: error opening lock file: %w", err)
	}
	return func() error {
		if err := lf.Close(); err != nil {
			return fmt.Errorf("lock: error releasing file lock: %w", err)
		}
		if err := os.Remove(lockfile); err != nil {
			return fmt.Errorf("lock: error removing lock file: %w", err)
		}
		return nil
	}, nil
}

// init creates all resources needed for the script to perform actions against
// remote resources like the Docker engine or remote storage locations.
func (s *script) init() error {
	s.ctx = context.Background()

	if err := godotenv.Load("/etc/backup.env"); err != nil {
		return fmt.Errorf("init: failed to load env file: %w", err)
	}

	_, err := os.Stat("/var/run/docker.sock")
	if !os.IsNotExist(err) {
		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return fmt.Errorf("init: failied to create docker client")
		}
		s.cli = cli
	}

	if bucket := os.Getenv("AWS_S3_BUCKET_NAME"); bucket != "" {
		s.bucket = bucket
		mc, err := minio.New(os.Getenv("AWS_ENDPOINT"), &minio.Options{
			Creds: credentials.NewStaticV4(
				os.Getenv("AWS_ACCESS_KEY_ID"),
				os.Getenv("AWS_SECRET_ACCESS_KEY"),
				"",
			),
			Secure: os.Getenv("AWS_ENDPOINT_PROTO") == "https",
		})
		if err != nil {
			return fmt.Errorf("init: error setting up minio client: %w", err)
		}
		s.mc = mc
	}
	file := os.Getenv("BACKUP_FILENAME")
	if file == "" {
		return errors.New("init: BACKUP_FILENAME not given")
	}
	s.file = path.Join("/tmp", file)
	return nil
}

// stopContainersAndRun stops all Docker containers that are marked as to being
// stopped during the backup and runs the given thunk. After returning, it makes
// sure containers are being restarted if required.
func (s *script) stopContainersAndRun(thunk func() error) error {
	if s.cli == nil {
		return nil
	}
	allContainers, err := s.cli.ContainerList(s.ctx, types.ContainerListOptions{
		Quiet: true,
	})
	if err != nil {
		return fmt.Errorf("stopContainersAndRun: error querying for containers: %w", err)
	}

	containersToStop, err := s.cli.ContainerList(s.ctx, types.ContainerListOptions{
		Quiet: true,
		Filters: filters.NewArgs(filters.KeyValuePair{
			Key: "label",
			Value: fmt.Sprintf(
				"docker-volume-backup.stop-during-backup=%s",
				os.Getenv("BACKUP_STOP_CONTAINER_LABEL"),
			),
		}),
	})

	if err != nil {
		return fmt.Errorf("stopContainersAndRun: error querying for containers to stop: %w", err)
	}
	fmt.Printf("Stopping %d out of %d running containers\n", len(containersToStop), len(allContainers))

	var stoppedContainers []types.Container
	var errors []error
	if len(containersToStop) != 0 {
		for _, container := range containersToStop {
			if err := s.cli.ContainerStop(s.ctx, container.ID, nil); err != nil {
				errors = append(errors, err)
			} else {
				stoppedContainers = append(stoppedContainers, container)
			}
		}
	}

	defer func() error {
		servicesRequiringUpdate := map[string]struct{}{}

		var restartErrors []error
		for _, container := range stoppedContainers {
			if swarmServiceName, ok := container.Labels["com.docker.swarm.service.name"]; ok {
				servicesRequiringUpdate[swarmServiceName] = struct{}{}
				continue
			}
			if err := s.cli.ContainerStart(s.ctx, container.ID, types.ContainerStartOptions{}); err != nil {
				restartErrors = append(restartErrors, err)
			}
		}

		if len(servicesRequiringUpdate) != 0 {
			services, _ := s.cli.ServiceList(s.ctx, types.ServiceListOptions{})
			for serviceName := range servicesRequiringUpdate {
				var serviceMatch swarm.Service
				for _, service := range services {
					if service.Spec.Name == serviceName {
						serviceMatch = service
						break
					}
				}
				if serviceMatch.ID == "" {
					return fmt.Errorf("stopContainersAndRun: Couldn't find service with name %s", serviceName)
				}
				serviceMatch.Spec.TaskTemplate.ForceUpdate = 1
				_, err := s.cli.ServiceUpdate(
					s.ctx, serviceMatch.ID,
					serviceMatch.Version, serviceMatch.Spec, types.ServiceUpdateOptions{},
				)
				if err != nil {
					restartErrors = append(restartErrors, err)
				}
			}
		}

		if len(restartErrors) != 0 {
			return fmt.Errorf(
				"stopContainersAndRun: %d error(s) restarting containers and services: %w",
				len(restartErrors),
				err,
			)
		}
		return nil
	}()

	var stopErr error
	if len(errors) != 0 {
		stopErr = fmt.Errorf(
			"stopContainersAndRun: %d errors stopping containers: %w",
			len(errors),
			err,
		)
	}
	if stopErr != nil {
		return stopErr
	}

	return thunk()
}

// takeBackup creates a tar archive of the configured backup location and
// saves it to disk.
func (s *script) takeBackup() error {
	outBytes, err := exec.Command("date", fmt.Sprintf("+%s", s.file)).Output()
	if err != nil {
		return fmt.Errorf("takeBackup: error formatting filename template: %w", err)
	}
	s.file = strings.TrimSpace(string(outBytes))
	if err := targz.Compress(os.Getenv("BACKUP_SOURCES"), s.file); err != nil {
		return fmt.Errorf("takeBackup: error compressing backup folder: %w", err)
	}
	return nil
}

// encryptBackup encrypts the backup file using PGP and the configured passphrase.
// In case no passphrase is given it returns early, leaving the backup file
//  untouched.
func (s *script) encryptBackup() error {
	passphrase := os.Getenv("GPG_PASSPHRASE")
	if passphrase == "" {
		return nil
	}

	buf := bytes.NewBuffer(nil)
	_, name := path.Split(s.file)
	pt, err := openpgp.SymmetricallyEncrypt(buf, []byte(passphrase), &openpgp.FileHints{
		IsBinary: true,
		FileName: name,
	}, nil)
	if err != nil {
		return fmt.Errorf("encryptBackup: error encrypting backup file: %w", err)
	}

	unencrypted, err := ioutil.ReadFile(s.file)
	if err != nil {
		pt.Close()
		return fmt.Errorf("encryptBackup: error reading unencrypted backup file: %w", err)
	}
	_, err = pt.Write(unencrypted)
	if err != nil {
		pt.Close()
		return fmt.Errorf("encryptBackup: error writing backup contents: %w", err)
	}
	pt.Close()

	gpgFile := fmt.Sprintf("%s.gpg", s.file)
	if err := ioutil.WriteFile(gpgFile, buf.Bytes(), os.ModeAppend); err != nil {
		return fmt.Errorf("encryptBackup: error writing encrypted version of backup: %w", err)
	}

	if err := os.Remove(s.file); err != nil {
		return fmt.Errorf("encryptBackup: error removing unencrpyted backup: %w", err)
	}
	s.file = gpgFile
	return nil
}

// copyBackup makes sure the backup file is copied to both local and remote locations
// as per the given configuration.
func (s *script) copyBackup() error {
	_, name := path.Split(s.file)
	if s.bucket != "" {
		_, err := s.mc.FPutObject(s.ctx, s.bucket, name, s.file, minio.PutObjectOptions{
			ContentType: "application/tar+gzip",
		})
		if err != nil {
			return fmt.Errorf("copyBackup: error uploading backup to remote storage: %w", err)
		}
	}

	if archive := os.Getenv("BACKUP_ARCHIVE"); archive != "" {
		if _, err := os.Stat(archive); !os.IsNotExist(err) {
			if err := copy(s.file, path.Join(archive, name)); err != nil {
				return fmt.Errorf("copyBackup: error copying file to local archive: %w", err)
			}
		}
	}
	return nil
}

// cleanBackup removes the backup file from disk.
func (s *script) cleanBackup() error {
	if err := os.Remove(s.file); err != nil {
		return fmt.Errorf("cleanBackup: error removing file: %w", err)
	}
	return nil
}

// pruneOldBackups rotates away backups from local and remote storages using
// the given configuration. In case the given configuration would delete all
// backups, it does nothing instead.
func (s *script) pruneOldBackups() error {
	retention := os.Getenv("BACKUP_RETENTION_DAYS")
	if retention == "" {
		return nil
	}
	retentionDays, err := strconv.Atoi(retention)
	if err != nil {
		return fmt.Errorf("pruneOldBackups: error parsing BACKUP_RETENTION_DAYS as int: %w", err)
	}
	sleepFor, err := time.ParseDuration(os.Getenv("BACKUP_PRUNING_LEEWAY"))
	if err != nil {
		return fmt.Errorf("pruneBackups: error parsing given leeway value: %w", err)
	}
	time.Sleep(sleepFor)

	deadline := time.Now().AddDate(0, 0, -retentionDays)

	if s.bucket != "" {
		candidates := s.mc.ListObjects(s.ctx, s.bucket, minio.ListObjectsOptions{
			WithMetadata: true,
			Prefix:       os.Getenv("BACKUP_PRUNING_PREFIX"),
		})

		var matches []minio.ObjectInfo
		for candidate := range candidates {
			if candidate.LastModified.Before(deadline) {
				matches = append(matches, candidate)
			}
		}

		if len(matches) != len(candidates) {
			objectsCh := make(chan minio.ObjectInfo)
			go func() {
				for _, candidate := range matches {
					objectsCh <- candidate
				}
				close(objectsCh)
			}()
			errChan := s.mc.RemoveObjects(s.ctx, s.bucket, objectsCh, minio.RemoveObjectsOptions{})
			var errors []error
			for result := range errChan {
				if result.Err != nil {
					errors = append(errors, result.Err)
				}
			}

			if len(errors) != 0 {
				return fmt.Errorf(
					"pruneOldBackups: %d errors removing files from remote storage: %w",
					len(errors),
					errors[0],
				)
			}
		} else if len(candidates) != 0 {
			fmt.Println("Refusing to delete all backups. Check your configuration.")
		}
	}

	if archive := os.Getenv("BACKUP_ARCHIVE"); archive != "" {
		candidates, err := filepath.Glob(
			path.Join(archive, fmt.Sprintf("%s%s", os.Getenv("BACKUP_PRUNING_PREFIX"), "*")),
		)
		if err != nil {
			return fmt.Errorf(
				"pruneOldBackups: error looking up matching files, starting with: %w", err,
			)
		}

		var matches []os.FileInfo
		for _, candidate := range candidates {
			fi, err := os.Stat(candidate)
			if err != nil {
				return fmt.Errorf(
					"pruneOldBackups: error calling stat on file %s: %w",
					candidate,
					err,
				)
			}

			if fi.ModTime().Before(deadline) {
				matches = append(matches, fi)
			}
		}

		if len(matches) != len(candidates) {
			var errors []error
			for _, candidate := range matches {
				if err := os.Remove(candidate.Name()); err != nil {
					errors = append(errors, err)
				}
			}
			if len(errors) != 0 {
				return fmt.Errorf(
					"pruneOldBackups: %d errors deleting local files, starting with: %w",
					len(errors),
					errors[0],
				)
			}
		} else if len(candidates) != 0 {
			fmt.Println("Refusing to delete all backups. Check your configuration.")
		}
	}
	return nil
}

func must(f func() error) func() {
	return func() {
		if err := f(); err != nil {
			panic(err)
		}
	}
}

func copy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	_, err = io.Copy(out, in)
	if err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
