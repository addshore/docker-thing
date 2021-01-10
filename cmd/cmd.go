package cmd

import (
	"bufio"
	"github.com/docker/go-connections/nat"
	"io/ioutil"
	"os/user"
	"github.com/docker/docker/api/types/mount"
	"strings"
	"github.com/docker/docker/api/types/strslice"
	"os"
	"io"
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"golang.org/x/crypto/ssh/terminal"
)

var fPort string
var fPull bool
var fUser string
var fUserMe bool
var fNoEntry bool
var fMountPwd bool
var fMountHome bool

func init() {
	// Defaults
	rootCmd.Flags().StringVarP(&fPort, "port", "", "", "Port mapping <host>:<container> eg. 8080:80")
	rootCmd.Flags().BoolVarP(&fNoEntry, "entry", "", true, "Use the default entrypoint. If entry=0 you must provide one")
	rootCmd.Flags().BoolVarP(&fMountPwd, "pwd", "", false, "Mount the PWD into the container (and set as working directory /pwd)")
	rootCmd.Flags().BoolVarP(&fMountHome, "home", "", false, "Mount the home directory of the user")
	rootCmd.Flags().StringVarP(&fUser, "user", "", "", "User override for the command")
	rootCmd.Flags().BoolVarP(&fUserMe, "me", "", false, "User override for the command, runs as current user")

	// Optional
	rootCmd.Flags().BoolVarP(&fPull, "pull", "", false, "Pull the docker image even if present")
}

// TODO allow port as an easy runtime option as ports may need to be exposed?
type RunNowOptions struct {
	Image		   string
	Pull			bool
	Cmd			 strslice.StrSlice
}

func RunNow(options RunNowOptions) (string, error) {
	cli, err := client.NewEnvClient()
	if err != nil {
		fmt.Println("Unable to create docker client")
		panic(err)
	}
	if Verbose {
		fmt.Println("Docker client loaded");
	}

	if(fPull){
		pull(cli,options)
	}

	// TODO more volumes?
	cont, err := containerCreate(cli, options)
	if Verbose {
		fmt.Println("Created container: " + cont.ID);
	}

	waiter, err := cli.ContainerAttach(context.Background(), cont.ID, types.ContainerAttachOptions{
		Stderr:	   true,
		Stdout:	   true,
		Stdin:		true,
		Stream:	   true,
	})

	go io.Copy(os.Stdout, waiter.Reader)
	go io.Copy(os.Stderr, waiter.Reader)
	// Stdin goes through a wrapper that listens for Ctrl+C. So we don't copy it here
	//go io.Copy(waiter.Conn, os.Stdin)

	if Verbose {
		fmt.Println("Starting container: " + cont.ID);
	}
	err = cli.ContainerStart(context.Background(), cont.ID, types.ContainerStartOptions{})
	if err != nil {
		fmt.Println("Error Starting container: " + cont.ID)
		panic(err)
	}

	fd := int(os.Stdin.Fd())
	var oldState *terminal.State
	if terminal.IsTerminal(fd) {
		oldState, err = terminal.MakeRaw(fd)
		if err != nil {
			if (Verbose){
				fmt.Println("Terminal: make raw ERROR")
			}
		}
		defer terminal.Restore(fd, oldState)

		// Wrapper around Stdin for the container, to detect Ctrl+C (as we are in raw mode)
		go func() {
			for {
				consoleReader := bufio.NewReaderSize(os.Stdin, 1)
				input, _ := consoleReader.ReadByte()
				// Ctrl-C = 3
				if input == 3 {
					if (Verbose){
						fmt.Println("Detected Ctrl+C, so telling docker to remove the container: " + cont.ID)
					}
					// Tell docker to forcefully remove the container
					cli.ContainerRemove( context.Background(), cont.ID, types.ContainerRemoveOptions{
						Force: true,
					} )
				}
				waiter.Conn.Write([]byte{input})
		}
		}()
	}

	statusCh, errCh := cli.ContainerWait(context.Background(), cont.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			panic(err)
		}
	case <-statusCh:
	}

	if Verbose {
		fmt.Println("Restoring terminal");
	}
	// TODO fixme, this call might be duplicated
	terminal.Restore(fd, oldState)
	fmt.Println("");

	if Verbose {
		fmt.Println("Ensuring Container Removal: " + cont.ID);
	}
	cli.ContainerRemove( context.Background(), cont.ID, types.ContainerRemoveOptions{
		Force: true,
	} )

	return cont.ID, nil
}

func containerCreate(cli *client.Client, options RunNowOptions) (container.ContainerCreateCreatedBody, error) {
	cont, err := containerCreateNoPullFallback(cli, options)
		if err != nil {
			if !strings.Contains(err.Error()," No such image") {
				fmt.Println("Error Creating")
				panic(err)
			}
			// Fallback pulling the image once
			if Verbose {
				fmt.Println("No image, so pulling");
			}
			pull(cli,options);
			return containerCreateNoPullFallback(cli, options)
		}
	return cont, err;
}

func containerCreateNoPullFallback(cli *client.Client, options RunNowOptions) (container.ContainerCreateCreatedBody, error) {
	if Verbose {
		fmt.Println("Creating container");
	}
	pwd, err := os.Getwd()
	if err != nil {
		fmt.Println(err)
		panic(err)
	}

	labels := make(map[string]string)
	labels["com.github/addshore/dockerit/created-app"] = "dockerit"
	labels["com.github/addshore/dockerit/created-command"] = "now"

	ContainerConfig := &container.Config{
		Image: options.Image,
		Cmd: options.Cmd,
		AttachStderr:true,
		AttachStdin: true,
		Tty:		 true,
		AttachStdout:true,
		OpenStdin:   true,
		Labels: labels,
	}

	var emptyMountsSliceEntry []mount.Mount
	HostConfig := &container.HostConfig{
		Mounts: emptyMountsSliceEntry,
		AutoRemove: true,
	}

	runAs := fUser
	if(fUserMe) {
		currentUser, err := user.Current()
		if err != nil {
			fmt.Println(err)
			panic(err)
		}
		runAs = currentUser.Username
	}
	if(len(runAs)>0) {
		usr, err := user.Lookup(runAs)
		if err != nil {
			fmt.Println(err)
			panic(err)
		}
		ContainerConfig.User = usr.Uid + ":" + usr.Gid
		if(fMountHome){
			// Check the home dir exists before mounting it
			_, err := os.Stat(usr.HomeDir)
			if os.IsNotExist(err) {
				fmt.Println("Homedir does not exist.")
				panic(err)
			}
			HostConfig.Mounts = append(
				HostConfig.Mounts,
				mount.Mount{
					Type:   mount.TypeBind,
					Source: usr.HomeDir,
					Target: usr.HomeDir,
				},
			)
		}
	}

	if(len(fPort)>0){
		splits := strings.Split(fPort, ":")
		hostPortString, containerPortString := splits[0], splits[1]
		containerPort := nat.Port(containerPortString+"/tcp")
		ContainerConfig.ExposedPorts = nat.PortSet{
			containerPort: {},
		}
		HostConfig.PortBindings = nat.PortMap{
			containerPort: []nat.PortBinding{
				{
					HostIP: "0.0.0.0",
					HostPort: hostPortString,
				},
			},
		}
	}
	if(fMountPwd){
		ContainerConfig.WorkingDir = "/pwd"
		HostConfig.Mounts = append(
			HostConfig.Mounts,
			mount.Mount{
				Type:   mount.TypeBind,
				Source: pwd,
				Target: "/pwd",
			},
		)
	}
	if(fNoEntry){
		var emptyStrSliceEntry []string
		ContainerConfig.Entrypoint = emptyStrSliceEntry
	}

	return cli.ContainerCreate(
		context.Background(),
		ContainerConfig,
		HostConfig,
		nil,
		nil,
		"",
		);
}

func pull(cli *client.Client, options RunNowOptions) {
	fmt.Println("Pulling image");
	r, err := cli.ImagePull(
		context.Background(),
		options.Image,
		types.ImagePullOptions{},
	)
	if err != nil {
		fmt.Println("Error Pulling")
		panic(err)
	}
	// TODO fixme this is super verbose...
	if Verbose {
		io.Copy(os.Stdout, r)
	} else {
		io.Copy(ioutil.Discard, r)
	}
}
