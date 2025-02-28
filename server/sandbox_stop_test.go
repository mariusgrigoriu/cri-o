package server_test

import (
	"context"

	"github.com/cri-o/cri-o/internal/oci"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	pb "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

// The actual test suite
var _ = t.Describe("PodSandboxStatus", func() {
	// Prepare the sut
	BeforeEach(func() {
		beforeEach()
		setupSUT()
	})

	AfterEach(afterEach)

	t.Describe("PodSandboxStatus", func() {
		It("should succeed", func() {
			// Given
			sut.SetRuntime(ociRuntimeMock)
			addContainerAndSandbox()
			testContainer.SetState(&oci.ContainerState{
				State: specs.State{Status: oci.ContainerStateRunning},
			})
			gomock.InOrder(
				cniPluginMock.EXPECT().TearDownPod(gomock.Any()).
					Return(nil),
				ociRuntimeMock.EXPECT().StopContainer(gomock.Any(),
					gomock.Any(), gomock.Any()).Return(nil),
				ociRuntimeMock.EXPECT().WaitContainerStateStopped(gomock.Any(),
					gomock.Any()).Return(nil),
				runtimeServerMock.EXPECT().StopContainer(gomock.Any()).
					Return(nil),
				ociRuntimeMock.EXPECT().UpdateContainerStatus(gomock.Any()).
					Return(nil),
			)

			// When
			response, err := sut.StopPodSandbox(context.Background(),
				&pb.StopPodSandboxRequest{PodSandboxId: testSandbox.ID()})

			// Then
			Expect(err).To(BeNil())
			Expect(response).NotTo(BeNil())
		})

		It("should succeed when container stop errors", func() {
			// Given
			sut.SetRuntime(ociRuntimeMock)
			addContainerAndSandbox()
			testContainer.SetState(&oci.ContainerState{
				State: specs.State{Status: oci.ContainerStateRunning},
			})
			gomock.InOrder(
				cniPluginMock.EXPECT().TearDownPod(gomock.Any()).
					Return(nil),
				ociRuntimeMock.EXPECT().StopContainer(gomock.Any(),
					gomock.Any(), gomock.Any()).Return(nil),
				ociRuntimeMock.EXPECT().WaitContainerStateStopped(gomock.Any(),
					gomock.Any()).Return(nil),
				runtimeServerMock.EXPECT().StopContainer(gomock.Any()).
					Return(t.TestError),
				ociRuntimeMock.EXPECT().UpdateContainerStatus(gomock.Any()).
					Return(nil),
			)

			// When
			response, err := sut.StopPodSandbox(context.Background(),
				&pb.StopPodSandboxRequest{PodSandboxId: testSandbox.ID()})

			// Then
			Expect(err).To(BeNil())
			Expect(response).NotTo(BeNil())
		})

		It("should fail when infra container wait for stop errors", func() {
			// Given
			sut.SetRuntime(ociRuntimeMock)
			addContainerAndSandbox()
			testContainer.SetState(&oci.ContainerState{
				State: specs.State{Status: oci.ContainerStateRunning},
			})
			gomock.InOrder(
				cniPluginMock.EXPECT().TearDownPod(gomock.Any()).
					Return(nil),
				ociRuntimeMock.EXPECT().StopContainer(gomock.Any(),
					gomock.Any(), gomock.Any()).Return(nil),
				ociRuntimeMock.EXPECT().WaitContainerStateStopped(gomock.Any(),
					gomock.Any()).Return(t.TestError),
			)

			// When
			response, err := sut.StopPodSandbox(context.Background(),
				&pb.StopPodSandboxRequest{PodSandboxId: testSandbox.ID()})

			// Then
			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
		})

		It("should fail when infra container stop errors", func() {
			// Given
			sut.SetRuntime(ociRuntimeMock)
			addContainerAndSandbox()
			testContainer.SetState(&oci.ContainerState{
				State: specs.State{Status: oci.ContainerStateRunning},
			})
			gomock.InOrder(
				cniPluginMock.EXPECT().TearDownPod(gomock.Any()).
					Return(nil),
				ociRuntimeMock.EXPECT().StopContainer(gomock.Any(),
					gomock.Any(), gomock.Any()).Return(t.TestError),
			)

			// When
			response, err := sut.StopPodSandbox(context.Background(),
				&pb.StopPodSandboxRequest{PodSandboxId: testSandbox.ID()})

			// Then
			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
		})

		It("should succeed with already stopped sandbox", func() {
			// Given
			addContainerAndSandbox()
			testSandbox.SetStopped()

			// When
			response, err := sut.StopPodSandbox(context.Background(),
				&pb.StopPodSandboxRequest{PodSandboxId: testSandbox.ID()})

			// Then
			Expect(err).To(BeNil())
			Expect(response).NotTo(BeNil())
		})

		It("should succeed with inavailable sandbox", func() {
			// Given
			// When
			response, err := sut.StopPodSandbox(context.Background(),
				&pb.StopPodSandboxRequest{PodSandboxId: "invalid"})

			// Then
			Expect(err).To(BeNil())
			Expect(response).NotTo(BeNil())
		})

		It("should fail when container is not stopped", func() {
			// Given
			addContainerAndSandbox()
			gomock.InOrder(
				cniPluginMock.EXPECT().TearDownPod(gomock.Any()).Return(t.TestError),
			)

			// When
			response, err := sut.StopPodSandbox(context.Background(),
				&pb.StopPodSandboxRequest{PodSandboxId: testSandbox.ID()})

			// Then
			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
		})

		It("should fail with empty sandbox ID", func() {
			// Given
			// When
			response, err := sut.StopPodSandbox(context.Background(),
				&pb.StopPodSandboxRequest{})

			// Then
			Expect(err).NotTo(BeNil())
			Expect(response).To(BeNil())
		})
	})
})
