package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/inhies/go-bytesize"
	"golang.org/x/sys/unix"
	"github.com/luckylittle/sizechecker/discord"
	"github.com/luckylittle/sizechecker/pushover"

)

func getUsedSpace(path string) (int64, error) {
	var size int64

	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})

	return size, err
}

func getAvailableSpace(dir string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func getNotificationTimestampFilePath(webhookURL string) string {
	hash := sha256.Sum256([]byte(webhookURL))
	hashStr := hex.EncodeToString(hash[:])
	return filepath.Join(os.TempDir(), "disk_space_checker_last_notification_"+hashStr)
}

func shouldSendNotification(webhookURL string, cooldown time.Duration) (bool, error) {
	filePath := getNotificationTimestampFilePath(webhookURL)
	//fmt.Println("Timestamp file path:", getNotificationTimestampFilePath(webhookURL))

	file, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return false, fmt.Errorf("error opening timestamp file: %v", err)
	}
	defer file.Close()

	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return false, fmt.Errorf("error acquiring file lock: %v", err)
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)

	data, err := io.ReadAll(file)
	if err != nil {
		return false, fmt.Errorf("error reading timestamp file: %v", err)
	}

	lastSentStr := string(bytes.TrimSpace(data))
	if lastSentStr == "" {
		return true, nil
	}

	lastSentUnix, err := strconv.ParseInt(lastSentStr, 10, 64)
	if err != nil {
		return true, nil
	}

	lastSentTime := time.Unix(lastSentUnix, 0)
	if time.Since(lastSentTime) >= cooldown {
		return true, nil
	}

	return false, nil
}

func updateNotificationTimestamp(webhookURL string) error {
	filePath := getNotificationTimestampFilePath(webhookURL)

	file, err := os.OpenFile(filePath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("error opening timestamp file: %v", err)
	}
	defer file.Close()

	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("error acquiring file lock: %v", err)
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)

	currentTime := strconv.FormatInt(time.Now().Unix(), 10)
	if _, err := file.WriteString(currentTime); err != nil {
		return fmt.Errorf("error writing timestamp file: %v", err)
	}

	return nil
}

func cleanSizeString(size string) string {
	return strings.ReplaceAll(size, " ", "")
}

func main() {
	limitFlag := flag.String("limit", "", "Limit size (e.g., 50GB). For 'u' runtype, it's the maximum allowed used space; for 'a', it's the minimum required free space.")
	runTypeFlag := flag.String("runtype", "", "'a' for available space check, 'u' for used space check")
	discordFlag := flag.String("discord", "", "Discord webhook URL for notifications (optional)")
	pushoverFlag := flag.String("o", "", "Trigger a Pushover notification. This requires `PUSHOVER_APITOKEN` and `PUSHOVER_USERKEY` to be set!")
	cooldownFlag := flag.Duration("cooldown", time.Minute, "Cooldown duration between notifications (e.g., 1m, 30s)")
	flag.Parse()

	if *limitFlag == "" {
		fmt.Println("Error: --limit flag is required.")
		flag.Usage()
		os.Exit(2)
	}

	if *runTypeFlag != "u" && *runTypeFlag != "a" {
		fmt.Println("Error: --runtype flag must be 'u' for used space or 'a' for available space.")
		flag.Usage()
		os.Exit(2)
	}

	if flag.NArg() < 1 {
		fmt.Println("Error: Directory path is required.")
		flag.Usage()
		os.Exit(2)
	}
	dir := flag.Arg(0)

	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Printf("Error resolving directory path: %v\n", err)
		os.Exit(2)
	}

	stat, err := os.Stat(absDir)
	if err != nil {
		fmt.Printf("Error accessing directory %s: %v\n", absDir, err)
		os.Exit(2)
	}
	if !stat.IsDir() {
		fmt.Printf("Error: Path %s is not a directory.\n", absDir)
		os.Exit(2)
	}

	limitBytes, err := bytesize.Parse(cleanSizeString(*limitFlag))
	if err != nil {
		fmt.Printf("Error parsing limit size: %v\n", err)
		os.Exit(2)
	}

	var (
		multiByteSize bytesize.ByteSize
		message       string
	)

	switch *runTypeFlag {
	case "u":
		usedBytes, err := getUsedSpace(absDir)
		if err != nil {
			fmt.Printf("Error getting used space: %v\n", err)
			os.Exit(2)
		}
		multiByteSize = bytesize.ByteSize(usedBytes)
		if multiByteSize >= limitBytes {
			message = fmt.Sprintf("Warning: %s used in %s, which is beyond the limit of %s.",
				multiByteSize, absDir, limitBytes)
			fmt.Println(message)
		} else {
			fmt.Printf("Used space is within acceptable limits: %s used of %s.\n", multiByteSize, limitBytes)
			os.Exit(0)
		}
	case "a":
		availableBytes, err := getAvailableSpace(absDir)
		if err != nil {
			fmt.Printf("Error getting available space: %v\n", err)
			os.Exit(2)
		}
		multiByteSize = bytesize.ByteSize(availableBytes)
		if multiByteSize < limitBytes {
			message = fmt.Sprintf("Warning: Only %s available in %s, which is below the limit of %s.",
				multiByteSize, absDir, limitBytes)
			fmt.Println(message)
		} else {
			fmt.Printf("Sufficient space: %s available.\n", multiByteSize)
			os.Exit(0)
		}
	default:
		fmt.Println("Error: Invalid --runtype value. Use 'u' for used space or 'a' for available space.")
		flag.Usage()
		os.Exit(2)
	}

	if *discordFlag != "" {
		sendNotification, err := shouldSendNotification(*discordFlag, *cooldownFlag)
		if err != nil {
			fmt.Printf("Error checking notification cooldown: %v\n", err)
		} else if sendNotification {
			if err := sendDiscordNotification(*discordFlag, message); err != nil {
				fmt.Printf("Error sending Discord notification: %v\n", err)
			} else {
				fmt.Println("Discord notification sent successfully.")
				if err := updateNotificationTimestamp(*discordFlag); err != nil {
					fmt.Printf("Error updating notification timestamp: %v\n", err)
				}
			}
		} else {
			fmt.Println("Notification not sent due to rate limiting.")
		}
	}

	if *pushoverFlag != "" {
		sendNotification, err := shouldSendNotification(*pushoverFlag, *cooldownFlag)
		if err != nil {
			fmt.Printf("Error checking notification cooldown: %v\n", err)
		} else if sendNotification {
			if err := pushoverNotification(*pushoverFlag, message); err != nil {
				fmt.Printf("Error sending Pusover notification: %v\n", err)
			} else {
				fmt.Println("Pushover notification sent successfully.")
				if err := updateNotificationTimestamp(*pushoverFlag); err != nil {
					fmt.Printf("Error updating notification timestamp: %v\n", err)
				}
			}
		} else {
			fmt.Println("Notification not sent due to rate limiting.")
		}
	}

	os.Exit(1)
}
