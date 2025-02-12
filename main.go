package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/alecthomas/kingpin"
	cpy "github.com/otiai10/copy"
)

var (
	version   string
	branch    string
	revision  string
	buildDate string
	goVersion = runtime.Version()
)

var (
	// flags
	action           = kingpin.Flag("action", "Any of the following actions: build, push, tag.").Envar("ESTAFETTE_EXTENSION_ACTION").String()
	repositories     = kingpin.Flag("repositories", "List of the repositories the image needs to be pushed to or tagged in.").Envar("ESTAFETTE_EXTENSION_REPOSITORIES").String()
	container        = kingpin.Flag("container", "Name of the container to build, defaults to app label if present.").Envar("ESTAFETTE_EXTENSION_CONTAINER").String()
	tags             = kingpin.Flag("tags", "List of tags the image needs to receive.").Envar("ESTAFETTE_EXTENSION_TAGS").String()
	path             = kingpin.Flag("path", "Directory to build docker container from, defaults to current working directory.").Default(".").OverrideDefaultFromEnvar("ESTAFETTE_EXTENSION_PATH").String()
	dockerfile       = kingpin.Flag("dockerfile", "Dockerfile to build, defaults to Dockerfile.").Default("Dockerfile").OverrideDefaultFromEnvar("ESTAFETTE_EXTENSION_DOCKERFILE").String()
	inlineDockerfile = kingpin.Flag("inline", "Dockerfile to build inlined.").Envar("ESTAFETTE_EXTENSION_INLINE").String()
	copy             = kingpin.Flag("copy", "List of files or directories to copy into the build directory.").Envar("ESTAFETTE_EXTENSION_COPY").String()
	target           = kingpin.Flag("target", "Specify which stage to target for multi-stage build.").Envar("ESTAFETTE_EXTENSION_TARGET").String()
	args             = kingpin.Flag("args", "List of build arguments to pass to the build.").Envar("ESTAFETTE_EXTENSION_ARGS").String()
	pushVersionTag   = kingpin.Flag("push-version-tag", "By default the version tag is pushed, so it can be promoted with a release, but if you don't want it you can disable it via this flag.").Default("true").Envar("ESTAFETTE_EXTENSION_PUSH_VERSION_TAG").Bool()

	gitName   = kingpin.Flag("git-name", "Repository name, used as application name if not passed explicitly and app label not being set.").Envar("ESTAFETTE_GIT_NAME").String()
	gitBranch = kingpin.Flag("git-branch", "Git branch to tag image with for improved caching.").Envar("ESTAFETTE_GIT_BRANCH").String()
	appLabel  = kingpin.Flag("app-name", "App label, used as application name if not passed explicitly.").Envar("ESTAFETTE_LABEL_APP").String()

	credentialsJSON = kingpin.Flag("credentials", "Container registry credentials configured at the CI server, passed in to this trusted extension.").Envar("ESTAFETTE_CREDENTIALS_CONTAINER_REGISTRY").String()
)

func main() {

	// parse command line parameters
	kingpin.Parse()

	// log to stdout and hide timestamp
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))

	// log startup message
	log.Printf("Starting estafette-extension-docker version %v...", version)

	// set defaults
	if *container == "" && *appLabel == "" && *gitName != "" {
		*container = *gitName
	}
	if *container == "" && *appLabel != "" {
		*container = *appLabel
	}

	// get api token from injected credentials
	var credentials []ContainerRegistryCredentials
	if *credentialsJSON != "" {
		err := json.Unmarshal([]byte(*credentialsJSON), &credentials)
		if err != nil {
			log.Fatal("Failed unmarshalling injected credentials: ", err)
		}
	}

	// validate inputs
	validateRepositories(*repositories)

	// split into arrays and set other variables
	var repositoriesSlice []string
	if *repositories != "" {
		repositoriesSlice = strings.Split(*repositories, ",")
	}
	var tagsSlice []string
	if *tags != "" {
		tagsSlice = strings.Split(*tags, ",")
	}
	var copySlice []string
	if *copy != "" {
		copySlice = strings.Split(*copy, ",")
	}
	var argsSlice []string
	if *args != "" {
		argsSlice = strings.Split(*args, ",")
	}
	estafetteBuildVersion := os.Getenv("ESTAFETTE_BUILD_VERSION")
	estafetteBuildVersionAsTag := tidyTag(estafetteBuildVersion)
	gitBranchAsTag := tidyTag(fmt.Sprintf("cache-%v", *gitBranch))

	switch *action {
	case "build":

		// minimal using defaults

		// image: extensions/docker:stable
		// action: build
		// repositories:
		// - extensions

		// with defaults:

		// path: .
		// container: ${ESTAFETTE_GIT_NAME}
		// dockerfile: Dockerfile

		// or use a more verbose version to override defaults

		// image: extensions/docker:stable
		// env: SOME_BUILD_ARG_ENVVAR
		// action: build
		// container: docker
		// dockerfile: Dockerfile
		// repositories:
		// - extensions
		// path: .
		// copy:
		// - Dockerfile
		// - /etc/ssl/certs/ca-certificates.crt
		// args:
		// - SOME_BUILD_ARG_ENVVAR

		// make build dir if it doesn't exist
		log.Printf("Ensuring build directory %v exists\n", *path)
		// runCommand("mkdir", []string{"-p", *path})
		if ok, _ := pathExists(*path); !ok {
			err := os.MkdirAll(*path, os.ModePerm)
			handleError(err)
		}

		if *inlineDockerfile != "" {
			// write inline dockerfile contents to Dockerfile in path
			targetDockerfile := filepath.Join(*path, "Dockerfile")

			log.Printf("Writing inline Dockerfile to %v\n", targetDockerfile)

			expandedInlineDockerfile := os.Expand(*inlineDockerfile, func(envar string) string {
				value := os.Getenv(envar)
				if value != "" {
					return value
				}

				return fmt.Sprintf("${%v}", envar)
			})

			err := ioutil.WriteFile(targetDockerfile, []byte(expandedInlineDockerfile), 0644)
			handleError(err)

			// ensure that any dockerfile param is ignored
			*dockerfile = "Dockerfile"
		} else {
			// add dockerfile to items to copy if path is non-default, and dockerfile isn't in the list to copy already, and if it is not there already
			if *path != "." && !contains(copySlice, *dockerfile) && filepath.Clean(filepath.Dir(*dockerfile)) != filepath.Clean(*path) {
				copySlice = append(copySlice, *dockerfile)
			}
		}

		// copy files/dirs from copySlice to build path
		for _, c := range copySlice {
			// runCommand("cp", []string{"-r", c, *path})

			fi, err := os.Stat(c)
			handleError(err)
			switch mode := fi.Mode(); {
			case mode.IsDir():
				log.Printf("Copying directory %v to %v\n", c, *path)
				err := cpy.Copy(c, filepath.Join(*path, filepath.Base(c)))
				handleError(err)

			case mode.IsRegular():
				log.Printf("Copying file %v to %v\n", c, *path)
				err := cpy.Copy(c, filepath.Join(*path, filepath.Base(c)))
				handleError(err)

			default:
				log.Fatalf("Unknown file mode %v for path %v", mode, c)
			}
		}

		// list directory content
		log.Printf("Listing directory %v content\n", *path)
		files, err := ioutil.ReadDir(*path)
		handleError(err)
		for _, f := range files {
			if f.IsDir() {
				log.Printf("- %v/", f.Name())
			} else {
				log.Printf("- %v", f.Name())
			}
		}

		// read dockerfile and find all images in FROM statements
		dockerfilePath := filepath.Join(*path, filepath.Base(*dockerfile))
		dockerfileContent, err := ioutil.ReadFile(dockerfilePath)
		handleError(err)

		fromImagePaths, err := getFromImagePathsFromDockerfile(dockerfileContent)
		handleError(err)

		// pull images in advance so we can log in to different repositories in the same registry (see https://github.com/moby/moby/issues/37569)
		for _, i := range fromImagePaths {
			loginIfRequired(credentials, i)
			log.Printf("Pulling container image %v\n", i)
			pullArgs := []string{
				"pull",
				i,
			}
			runCommand("docker", pullArgs)
		}

		// login to registry for destination container image
		containerPath := fmt.Sprintf("%v/%v:%v", repositoriesSlice[0], *container, estafetteBuildVersionAsTag)
		loginIfRequired(credentials, containerPath)
		cacheContainerPath := fmt.Sprintf("%v/%v:%v", repositoriesSlice[0], *container, gitBranchAsTag)

		log.Printf("Pulling docker image %v to use as cache during build...\n", cacheContainerPath)
		pullArgs := []string{
			"pull",
			cacheContainerPath,
		}
		// ignore if it fails
		runCommandExtended("docker", pullArgs)

		// build docker image
		log.Printf("Building docker image %v...\n", containerPath)

		log.Println("")
		// runCommand("cat", []string{dockerfilePath})
		dockerfileContent, err = ioutil.ReadFile(dockerfilePath)
		handleError(err)
		fmt.Println(string(dockerfileContent))
		log.Println("")

		args := []string{
			"build",
		}
		args = append(args, "--tag", cacheContainerPath)
		for _, r := range repositoriesSlice {
			args = append(args, "--tag", fmt.Sprintf("%v/%v:%v", r, *container, estafetteBuildVersionAsTag))
			for _, t := range tagsSlice {
				if r == repositoriesSlice[0] && (t == estafetteBuildVersionAsTag || t == gitBranchAsTag) {
					continue
				}
				args = append(args, "--tag", fmt.Sprintf("%v/%v:%v", r, *container, t))
			}
		}
		if *target != "" {
			args = append(args, "--target", *target)
		}
		for _, a := range argsSlice {
			argValue := os.Getenv(a)
			args = append(args, "--build-arg", fmt.Sprintf("%v=%v", a, argValue))
		}

		args = append(args, "--cache-from", cacheContainerPath)
		args = append(args, "--file", filepath.Join(*path, filepath.Base(*dockerfile)))
		args = append(args, *path)
		runCommand("docker", args)

	case "push":

		// image: extensions/docker:stable
		// action: push
		// container: docker
		// repositories:
		// - extensions
		// tags:
		// - dev

		sourceContainerPath := fmt.Sprintf("%v/%v:%v", repositoriesSlice[0], *container, estafetteBuildVersionAsTag)
		cacheContainerPath := fmt.Sprintf("%v/%v:%v", repositoriesSlice[0], *container, gitBranchAsTag)

		// push each repository + tag combination
		for i, r := range repositoriesSlice {

			targetContainerPath := fmt.Sprintf("%v/%v:%v", r, *container, estafetteBuildVersionAsTag)

			if i > 0 {
				// tag container with default tag (it already exists for the first repository)
				log.Printf("Tagging container image %v\n", targetContainerPath)
				tagArgs := []string{
					"tag",
					sourceContainerPath,
					targetContainerPath,
				}
				err := exec.Command("docker", tagArgs...).Run()
				handleError(err)
			}

			loginIfRequired(credentials, targetContainerPath)

			if *pushVersionTag {
				// push container with default tag
				log.Printf("Pushing container image %v\n", targetContainerPath)
				pushArgs := []string{
					"push",
					targetContainerPath,
				}
				runCommand("docker", pushArgs)
			} else {
				log.Println("Skipping pushing version tag, because pushVersionTag is set to false; this make promoting a version to a tag at a later stage impossible!")
			}

			if !*pushVersionTag && len(tagsSlice) == 0 {
				log.Fatal("When setting pushVersionTag to false you need at least one tag")
			}

			log.Printf("Pushing cache container image %v\n", cacheContainerPath)
			pushArgs := []string{
				"push",
				cacheContainerPath,
			}
			runCommand("docker", pushArgs)

			// push additional tags
			for _, t := range tagsSlice {

				if r == repositoriesSlice[0] && (t == estafetteBuildVersionAsTag || t == gitBranchAsTag) {
					continue
				}

				targetContainerPath := fmt.Sprintf("%v/%v:%v", r, *container, t)

				// tag container with additional tag
				log.Printf("Tagging container image %v\n", targetContainerPath)
				tagArgs := []string{
					"tag",
					sourceContainerPath,
					targetContainerPath,
				}
				runCommand("docker", tagArgs)

				loginIfRequired(credentials, targetContainerPath)

				log.Printf("Pushing container image %v\n", targetContainerPath)
				pushArgs := []string{
					"push",
					targetContainerPath,
				}
				runCommand("docker", pushArgs)
			}
		}

	case "tag":

		// image: extensions/docker:stable
		// action: tag
		// container: docker
		// repositories:
		// - extensions
		// tags:
		// - stable
		// - latest

		sourceContainerPath := fmt.Sprintf("%v/%v:%v", repositoriesSlice[0], *container, estafetteBuildVersionAsTag)

		loginIfRequired(credentials, sourceContainerPath)

		// pull source container first
		log.Printf("Pulling container image %v\n", sourceContainerPath)
		pullArgs := []string{
			"pull",
			sourceContainerPath,
		}
		runCommand("docker", pullArgs)

		// push each repository + tag combination
		for i, r := range repositoriesSlice {

			targetContainerPath := fmt.Sprintf("%v/%v:%v", r, *container, estafetteBuildVersionAsTag)

			if i > 0 {
				// tag container with default tag
				log.Printf("Tagging container image %v\n", targetContainerPath)
				tagArgs := []string{
					"tag",
					sourceContainerPath,
					targetContainerPath,
				}
				runCommand("docker", tagArgs)

				loginIfRequired(credentials, targetContainerPath)

				// push container with default tag
				log.Printf("Pushing container image %v\n", targetContainerPath)
				pushArgs := []string{
					"push",
					targetContainerPath,
				}
				runCommand("docker", pushArgs)
			}

			// push additional tags
			for _, t := range tagsSlice {

				targetContainerPath := fmt.Sprintf("%v/%v:%v", r, *container, t)

				// tag container with additional tag
				log.Printf("Tagging container image %v\n", targetContainerPath)
				tagArgs := []string{
					"tag",
					sourceContainerPath,
					targetContainerPath,
				}
				runCommand("docker", tagArgs)

				loginIfRequired(credentials, targetContainerPath)

				log.Printf("Pushing container image %v\n", targetContainerPath)
				pushArgs := []string{
					"push",
					targetContainerPath,
				}
				runCommand("docker", pushArgs)
			}
		}

	default:
		log.Fatal("Set `command: <command>` on this step to build, push or tag")
	}
}

func validateRepositories(repositories string) {
	if repositories == "" {
		log.Fatal("Set `repositories:` to list at least one `- <repository>` (for example like `- extensions`)")
	}
}

func getCredentialsForContainers(credentials []ContainerRegistryCredentials, containerImages []string) map[string]*ContainerRegistryCredentials {

	filteredCredentialsMap := make(map[string]*ContainerRegistryCredentials, 0)

	if credentials != nil {
		// loop all container images
		for _, ci := range containerImages {
			containerImageSlice := strings.Split(ci, "/")
			containerRepo := strings.Join(containerImageSlice[:len(containerImageSlice)-1], "/")

			if _, ok := filteredCredentialsMap[containerRepo]; ok {
				// credentials for this repo were added before, check next container image
				continue
			}

			// find the credentials matching the container image
			for _, credential := range credentials {
				if containerRepo == credential.AdditionalProperties.Repository {
					// this one matches, add it to the map
					filteredCredentialsMap[credential.AdditionalProperties.Repository] = &credential
					break
				}
			}
		}
	}

	return filteredCredentialsMap
}

var (
	imagesFromDockerFileRegex *regexp.Regexp
)

func getFromImagePathsFromDockerfile(dockerfileContent []byte) ([]string, error) {

	containerImages := []string{}

	if imagesFromDockerFileRegex == nil {
		imagesFromDockerFileRegex = regexp.MustCompile(`(?im)^FROM\s*([^\s]+)(\s*AS\s[a-zA-Z0-9]+)?\s*$`)
	}

	matches := imagesFromDockerFileRegex.FindAllStringSubmatch(string(dockerfileContent), -1)

	if len(matches) > 0 {
		for _, m := range matches {
			if len(m) > 1 {
				// check if it's not an official docker hub image
				if strings.Count(m[1], "/") != 0 {
					containerImages = append(containerImages, m[1])
				}
			}
		}
	}

	return containerImages, nil
}

func loginIfRequired(credentials []ContainerRegistryCredentials, containerImages ...string) {

	log.Printf("Filtering credentials for images %v\n", containerImages)

	// retrieve all credentials
	filteredCredentialsMap := getCredentialsForContainers(credentials, containerImages)

	log.Printf("Filtered %v container-registry credentials down to %v\n", len(credentials), len(filteredCredentialsMap))

	if filteredCredentialsMap != nil {
		for _, c := range filteredCredentialsMap {
			if c != nil {
				log.Printf("Logging in to repository '%v'\n", c.AdditionalProperties.Repository)
				loginArgs := []string{
					"login",
					"--username",
					c.AdditionalProperties.Username,
					"--password",
					c.AdditionalProperties.Password,
				}

				repositorySlice := strings.Split(c.AdditionalProperties.Repository, "/")
				if len(repositorySlice) > 1 {
					server := repositorySlice[0]
					loginArgs = append(loginArgs, server)
				}

				err := exec.Command("docker", loginArgs...).Run()
				handleError(err)
			}
		}
	}
}

func handleError(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func runCommand(command string, args []string) {
	err := runCommandExtended(command, args)
	handleError(err)
}

func runCommandExtended(command string, args []string) error {
	log.Printf("Running command '%v %v'...", command, strings.Join(args, " "))
	cmd := exec.Command(command, args...)
	cmd.Dir = "/estafette-work"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return err
}

func tidyTag(tag string) string {
	// A tag name must be valid ASCII and may contain lowercase and uppercase letters, digits, underscores, periods and dashes.
	tag = regexp.MustCompile(`[^a-zA-Z0-9_.\-]+`).ReplaceAllString(tag, "-")

	// A tag name may not start with a period or a dash
	tag = regexp.MustCompile(`$[_.\-]+`).ReplaceAllString(tag, "")

	// and may contain a maximum of 128 characters.
	if len(tag) > 128 {
		tag = tag[:128]
	}

	return tag
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}
