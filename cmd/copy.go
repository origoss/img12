// Copyright © 2018 NAME HERE <EMAIL ADDRESS>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"fmt"

	"bufio"
	"context"
	"github.com/spf13/cobra"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var sizeRegexp = regexp.MustCompile("^fully allocated size: ([[:digit:]]*)$")

func imageSize(sourceImage string) (int, error) {
	var err error
	fmt.Printf("qemu-img measure %s\n", sourceImage)
	cmd := exec.Command("qemu-img", "measure", sourceImage)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, err
	}

	if err = cmd.Start(); err != nil {
		return 0, err
	}
	if _, err = io.Copy(os.Stdout, stderr); err != nil {
		return 0, err
	}
	r := bufio.NewReader(stdout)
	if _, _, err := r.ReadLine(); err != nil {
		return 0, err
	}

	line, _, err := r.ReadLine()
	if err != nil {
		return 0, err
	}

	ss := sizeRegexp.FindStringSubmatch(string(line))
	if len(ss) < 2 {
		return 0, fmt.Errorf("regexp does not match")
	}

	if err = cmd.Wait(); err != nil {
		return 0, err
	}
	imageSize, err := strconv.Atoi(ss[1])
	if err != nil {
		return 0, err
	}
	return imageSize, nil
}

func removeVolume(ctx context.Context, volumeGroupLV string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	fmt.Printf("lvremove -vf %s\n", volumeGroupLV)
	cmd := exec.CommandContext(ctx, "lvremove", "-yf", volumeGroupLV)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return (err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return (err)
	}
	if err = cmd.Start(); err != nil {
		return (err)
	}
	if _, err = io.Copy(os.Stdout, stdout); err != nil {
		return err
	}
	if _, err = io.Copy(os.Stdout, stderr); err != nil {
		return err
	}
	if err = cmd.Wait(); err != nil {
		switch e := err.(type) {
		case *exec.ExitError:
			if e.Error() == "exit status 5" {
				break
			}
			return err
		default:
			return err
		}
	}

	return nil

}
func createVolume(ctx context.Context, sourceImage, volumeGroupLV string) error {
	imageSize, err := imageSize(sourceImage)
	splits := strings.SplitN(volumeGroupLV, "/", 2)
	if len(splits) < 2 {
		return fmt.Errorf("volume name shall be defined as VOLGROUP/LVNAME")
	}
	volGroupName := splits[0]
	logVolName := splits[1]
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	fmt.Printf("lvcreate -L %db -n %s %s\n", imageSize, logVolName, volGroupName)
	cmd := exec.CommandContext(ctx,
		"lvcreate",
		"-L",
		fmt.Sprintf("%db", imageSize),
		"-n",
		logVolName,
		volGroupName,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	if _, err = io.Copy(os.Stdout, stdout); err != nil {
		return err
	}
	if _, err = io.Copy(os.Stdout, stderr); err != nil {
		return err
	}
	if err = cmd.Wait(); err != nil {
		return err
	}
	return nil
}

func copyFromImage(ctx context.Context, sourceImage, volumeGroupLV string) error {
	volumeGroupLVDev := fmt.Sprintf("/dev/%s", volumeGroupLV)
	fmt.Printf("qemu-img convert %s -O raw %s\n", sourceImage, volumeGroupLVDev)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx,
		"qemu-img",
		"convert",
		sourceImage,
		"-O", "raw",
		volumeGroupLVDev,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return (err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return (err)
	}
	start := time.Now()
	if err = cmd.Start(); err != nil {
		return (err)
	}
	if _, err = io.Copy(os.Stdout, stdout); err != nil {
		return err
	}
	if _, err = io.Copy(os.Stdout, stderr); err != nil {
		return err
	}
	if err = cmd.Wait(); err != nil {
		return err
	}
	fmt.Printf("%s copied to %s (%s)\n", sourceImage, volumeGroupLV, time.Since(start))
	return nil
}

func worker(ctx context.Context, sourceImage string, volGroupLVChan <-chan string, errChan chan<- error) {
	wg := ctx.Value(ctxParamWaitGroup).(*sync.WaitGroup)
	for lv := range volGroupLVChan {
		if err := removeVolume(ctx, lv); err != nil {
			errChan <- err
			continue
		}
		if err := createVolume(ctx, sourceImage, lv); err != nil {
			errChan <- err
			continue
		}
		if err := copyFromImage(ctx, sourceImage, lv); err != nil {
			errChan <- err
			continue
		}
	}
	wg.Done()
}

func errHandler(errChan <-chan error) {
	for err := range errChan {
		fmt.Println(err)
	}
}

type ctxParam int

const (
	ctxParamWaitGroup ctxParam = iota
)

// copyCmd represents the copy command
var copyCmd = &cobra.Command{
	Use:   "copy",
	Short: "Copy a KVM image to logical volumes",
	Long:  `Copy a KVM image to logical volumes.`,
	Args:  cobra.MinimumNArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("copy called")
		lvChan := make(chan string)
		errChan := make(chan error)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wg := sync.WaitGroup{}
		for i := 0; i < 4; i++ {
			wg.Add(1)
			workerCtx := context.WithValue(ctx, ctxParamWaitGroup, &wg)
			go worker(workerCtx, args[0], lvChan, errChan)
		}
		go errHandler(errChan)
		for _, lv := range args[1:] {
			lvChan <- lv
		}
		close(lvChan)
		wg.Wait()
		close(errChan)
	},
}

func init() {
	rootCmd.AddCommand(copyCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// copyCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// copyCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
