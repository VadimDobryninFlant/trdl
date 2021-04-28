package trdl

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/go-git/go-git/v5"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/logical"

	trdlGit "github.com/werf/vault-plugin-secrets-trdl/pkg/git"
	"github.com/werf/vault-plugin-secrets-trdl/pkg/publisher"
)

const (
	containerSourceDir    = "/git"
	containerArtifactsDir = "/result"

	serviceDirInContextTar        = ".trdl"
	serviceDockerfileInContextTar = ".trdl/Dockerfile"
)

var (
	artifactsTarStartReadCode = []byte("1EA01F53E0277546E1B17267F29A60B3CD4DC12744C2FA2BF0897065DC3749F3")
	artifactsTarStopReadCode  = []byte("A2F00DB0DEE3540E246B75B872D64773DF67BC51C5D36D50FA6978E2FFDA7D43")
)

func pathRelease(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: `release$`,
		Fields: map[string]*framework.FieldSchema{
			"git-tag": {
				Type:        framework.TypeString,
				Description: "Project git repository tag which should be released (required)",
			},
			"command": {
				Type:        framework.TypeString,
				Description: "Run specified command in the root of project git repository tag (required)",
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathRelease,
				Summary:  pathReleaseHelpSyn,
			},
		},

		HelpSynopsis:    pathReleaseHelpSyn,
		HelpDescription: pathReleaseHelpDesc,
	}
}

func (b *backend) pathRelease(ctx context.Context, _ *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	gitTag := d.Get("git-tag").(string)
	if gitTag == "" {
		return logical.ErrorResponse("missing git-tag"), nil
	}

	command := d.Get("command").(string)
	if command == "" {
		return logical.ErrorResponse("missing command"), nil
	}

	url := "https://github.com/werf/trdl-test-project.git" // TODO: get url from vault storage

	awsAccessKeyID, err := GetAwsAccessKeyID() // TODO: get from vault storage, should be configured by the user
	if err != nil {
		return nil, fmt.Errorf("unable to get aws access key ID: %s", err)
	}

	awsSecretAccessKey, err := GetAwsSecretAccessKey() // TODO: get from vault storage, should be configured by the user
	if err != nil {
		return nil, fmt.Errorf("unable to get aws secret access key: %s", err)
	}

	// TODO: get from vault storage, should be configured by the user
	awsConfig := &aws.Config{
		Endpoint:    aws.String("https://storage.yandexcloud.net"),
		Region:      aws.String("ru-central1"),
		Credentials: credentials.NewStaticCredentials(awsAccessKeyID, awsSecretAccessKey, ""),
	}

	// TODO: get from vault storage, should be generated automatically by the plugin, user never has an access to these private keys
	publisherKeys, err := LoadFixturePublisherKeys()
	if err != nil {
		return nil, fmt.Errorf("error loading publisher fixture keys")
	}

	// Initialize repository before any operations, to ensure everything is setup correctly before building artifact
	publisherRepository, err := publisher.NewRepositoryWithOptions(
		publisher.S3Options{AwsConfig: awsConfig, BucketName: "trdl-test-project"}, // TODO: get from vault storage, should be configured by the user
		publisher.TufRepoOptions{PrivKeys: publisherKeys},
	)
	if err != nil {
		return nil, fmt.Errorf("error initializing publisher repository: %s", err)
	}

	taskID := b.releaseTasks.RunQueuedTask(func(ctx context.Context) error {
		stderr := os.NewFile(uintptr(syscall.Stderr), "/dev/stderr")

		fmt.Fprintf(stderr, "Started task\n")

		gitRepo, err := cloneGitRepository(url, gitTag)
		if err != nil {
			return fmt.Errorf("unable to clone git repository: %s", err)
		}

		fmt.Fprintf(stderr, "Cloned git repo\n")

		// TODO: get pgp public keys from vault storage, should be configured by the user
		var pgpPublicKeys []string
		// TODO: get requiredNumberOfVerifiedSignatures (required number of signatures made with different keys) from vault storage, should be configured by the user
		var requiredNumberOfVerifiedSignatures int

		if err := trdlGit.VerifyTagSignatures(gitRepo, gitTag, pgpPublicKeys, requiredNumberOfVerifiedSignatures); err != nil {
			return fmt.Errorf("signature verification failed: %s", err)
		}

		fromImage := "golang:latest"     // TODO: get fromImage from vault storage
		runCommands := []string{command} // TODO: get commands from vault storage or trdl config from git repository=

		tarReader, tarWriter := io.Pipe()
		if err := buildReleaseArtifacts(ctx, tarWriter, gitRepo, fromImage, runCommands); err != nil {
			return fmt.Errorf("unable to build release artifacts: %s", err)
		}

		fmt.Fprintf(stderr, "Created tar\n")

		var fileNames []string
		{ // TODO: publisher code here
			twArtifacts := tar.NewReader(tarReader)
			for {
				hdr, err := twArtifacts.Next()

				if err == io.EOF {
					break
				}

				if err != nil {
					return fmt.Errorf("error reading next tar artifact header: %s", err)
				}

				if hdr.Typeflag != tar.TypeDir {
					fmt.Fprintf(stderr, "Publishing %q into the tuf repo ...\n", hdr.Name)

					if err := publisher.PublishReleaseTarget(ctx, publisherRepository, gitTag, hdr.Name, twArtifacts); err != nil {
						return fmt.Errorf("unable to publish release target %q: %s", hdr.Name, err)
					}

					fmt.Fprintf(stderr, "Published %q into the tuf repo\n", hdr.Name)

					fileNames = append(fileNames, hdr.Name)
				}
			}

			if err := publisherRepository.Commit(ctx); err != nil {
				return fmt.Errorf("unable to commit new tuf repository state: %s", err)
			}

			fmt.Fprintf(stderr, "Tuf repo commit done\n")
		}

		return nil
	})

	return &logical.Response{
		Data: map[string]interface{}{
			"TaskID": taskID,
		},
	}, nil
}

func cloneGitRepository(url string, gitTag string) (*git.Repository, error) {
	cloneGitOptions := trdlGit.CloneOptions{
		TagName:           gitTag,
		RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
	}

	gitRepo, err := trdlGit.CloneInMemory(url, cloneGitOptions)
	if err != nil {
		return nil, err
	}

	return gitRepo, nil
}

func buildReleaseArtifacts(ctx context.Context, tarWriter *io.PipeWriter, gitRepo *git.Repository, fromImage string, runCommands []string) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("unable to create docker client: %s", err)
	}

	contextReader, contextWriter := io.Pipe()
	go func() {
		if err := writeContextTar(contextWriter, gitRepo, fromImage, runCommands); err != nil {
			if closeErr := contextWriter.CloseWithError(err); closeErr != nil {
				panic(closeErr)
			}
			return
		}

		if err := contextWriter.Close(); err != nil {
			panic(err)
		}
	}()

	response, err := cli.ImageBuild(ctx, contextReader, types.ImageBuildOptions{
		Dockerfile:  serviceDockerfileInContextTar,
		PullParent:  true,
		NoCache:     true,
		Remove:      true,
		ForceRemove: true,
		Version:     types.BuilderV1,
	})
	if err != nil {
		return fmt.Errorf("unable to run docker image build: %s", err)
	}

	processFromImageBuildResponse(response, tarWriter)

	return nil
}

func writeContextTar(contextWriter io.Writer, gitRepo *git.Repository, fromImage string, runCommands []string) error {
	tw := tar.NewWriter(contextWriter)
	writeHeaderFunc := func(entryName string, header *tar.Header) error {
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("unable to write tar entry %q header: %s", entryName, err)
		}

		return nil
	}

	if err := trdlGit.ForEachWorktreeFile(gitRepo, func(path, link string, fileReader io.Reader, info os.FileInfo) error {
		size := info.Size()

		// The size field is the size of the file in bytes; linked files are archived with this field specified as zero
		if link != "" {
			size = 0
		}

		if err := writeHeaderFunc(path, &tar.Header{
			Format:     tar.FormatGNU,
			Name:       path,
			Linkname:   link,
			Size:       size,
			Mode:       int64(info.Mode()),
			ModTime:    time.Now(),
			AccessTime: time.Now(),
			ChangeTime: time.Now(),
		}); err != nil {
			return err
		}

		if link == "" {
			_, err := io.Copy(tw, fileReader)
			if err != nil {
				return fmt.Errorf("unable to write tar entry %q data: %s", path, err)
			}
		}

		return nil
	}); err != nil {
		return err
	}

	dockerfileData := generateServiceDockerfile(fromImage, runCommands)
	if err := writeHeaderFunc(serviceDockerfileInContextTar, &tar.Header{
		Format:     tar.FormatGNU,
		Name:       serviceDockerfileInContextTar,
		Size:       int64(len(dockerfileData)),
		Mode:       int64(os.ModePerm),
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
	}); err != nil {
		return err
	}

	if _, err := tw.Write(dockerfileData); err != nil {
		return fmt.Errorf("unable to write tar entry %q data: %s", serviceDockerfileInContextTar, err)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("unable to close tar writer: %s", err)
	}

	return nil
}

func generateServiceDockerfile(fromImage string, runCommands []string) []byte {
	var data []byte
	addLineFunc := func(line string) {
		data = append(data, []byte(line+"\n")...)
	}

	addLineFunc(fmt.Sprintf("FROM %s", fromImage))

	// copy source code and set workdir for the following docker instructions
	addLineFunc(fmt.Sprintf("COPY . %s", containerSourceDir))
	addLineFunc(fmt.Sprintf("WORKDIR %s", containerSourceDir))

	// remove service data from user's context
	addLineFunc(fmt.Sprintf("RUN %s", fmt.Sprintf("rm -rf %s", serviceDirInContextTar)))

	// create empty dir for release artifacts
	addLineFunc(fmt.Sprintf("RUN %s", fmt.Sprintf("mkdir %s", containerArtifactsDir)))

	// run user's build commands
	for _, command := range runCommands {
		addLineFunc(fmt.Sprintf("RUN %s", command))
	}

	// tar result files to stdout (with control messages for a receiver)
	serviceRunCommands := []string{
		fmt.Sprintf("echo -n $(echo -n '%s' | base64 -d)", base64.StdEncoding.EncodeToString(artifactsTarStartReadCode)),
		fmt.Sprintf("tar c -C %s . | base64", containerArtifactsDir),
		fmt.Sprintf("echo -n $(echo -n '%s' | base64 -d)", base64.StdEncoding.EncodeToString(artifactsTarStopReadCode)),
	}
	addLineFunc("RUN " + strings.Join(serviceRunCommands, " && "))

	return data
}

func processFromImageBuildResponse(response types.ImageBuildResponse, tarWriter *io.PipeWriter) {
	r, w := io.Pipe()

	go func() {
		if err := readTarFromImageBuildResponse(response, w); err != nil {
			if closeErr := w.CloseWithError(err); closeErr != nil {
				panic(closeErr)
			}
			return
		}

		if err := w.Close(); err != nil {
			panic(err)
		}
	}()

	go func() {
		decoder := base64.NewDecoder(base64.StdEncoding, r)
		if _, err := io.Copy(tarWriter, decoder); err != nil {
			if closeErr := tarWriter.CloseWithError(err); closeErr != nil {
				panic(closeErr)
			}
			return
		}

		if err := w.Close(); err != nil {
			panic(err)
		}
	}()
}

func readTarFromImageBuildResponse(response types.ImageBuildResponse, writer io.Writer) error {
	dec := json.NewDecoder(response.Body)

	const (
		checkingStartCode = iota
		processingStartCode
		processingDataAndCheckingStopCode
		processingStopCode
	)
	currentState := checkingStartCode
	var codeCursor int
	var bufferedData []byte

	for {
		var jm jsonmessage.JSONMessage
		if err := dec.Decode(&jm); err != nil {
			if err == io.EOF {
				return nil
			}

			return fmt.Errorf("unable to decode message from docker daemon: %s", err)
		}

		if jm.Error != nil {
			return jm.Error
		}

		msg := jm.Stream
		if msg != "" {
			for _, b := range []byte(msg) {
				switch currentState {
				case checkingStartCode:
					if b == artifactsTarStartReadCode[0] {
						currentState = processingStartCode
						codeCursor++
					}
				case processingStartCode:
					if b == artifactsTarStartReadCode[codeCursor] {
						if len(artifactsTarStartReadCode) > codeCursor+1 {
							codeCursor++
						} else {
							currentState = processingDataAndCheckingStopCode
							codeCursor = 0
						}
					} else {
						currentState = checkingStartCode
						codeCursor = 0
					}
				case processingDataAndCheckingStopCode:
					bufferedData = append(bufferedData, b)

					if b == artifactsTarStopReadCode[0] {
						currentState = processingStopCode
						codeCursor++
						continue
					}

					if _, err := writer.Write(bufferedData); err != nil {
						return err
					}

					bufferedData = nil
				case processingStopCode:
					bufferedData = append(bufferedData, b)

					if b == artifactsTarStopReadCode[codeCursor] {
						if len(artifactsTarStopReadCode) > codeCursor+1 {
							codeCursor++
						} else {
							return nil
						}
					} else {
						currentState = processingDataAndCheckingStopCode
						codeCursor = 0
					}
				}
			}
		}
	}
}

const (
	pathReleaseHelpSyn = `
	Performs release of project.
	`

	pathReleaseHelpDesc = `
	Performs release of project by the specified git tag.
	Provided command should prepare release artifacts in the /result directory, which will be published into the TUF repository.
	`
)
