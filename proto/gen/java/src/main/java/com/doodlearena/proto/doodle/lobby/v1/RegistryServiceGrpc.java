package com.doodlearena.proto.doodle.lobby.v1;

import static io.grpc.MethodDescriptor.generateFullMethodName;

/**
 * <pre>
 * RegistryService is served by the lobby and called by every game server and relay.
 * </pre>
 */
@io.grpc.stub.annotations.GrpcGenerated
public final class RegistryServiceGrpc {

  private RegistryServiceGrpc() {}

  public static final java.lang.String SERVICE_NAME = "doodle.lobby.v1.RegistryService";

  // Static method descriptors that strictly reflect the proto.
  private static volatile io.grpc.MethodDescriptor<com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest,
      com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse> getHeartbeatMethod;

  @io.grpc.stub.annotations.RpcMethod(
      fullMethodName = SERVICE_NAME + '/' + "Heartbeat",
      requestType = com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest.class,
      responseType = com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse.class,
      methodType = io.grpc.MethodDescriptor.MethodType.UNARY)
  public static io.grpc.MethodDescriptor<com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest,
      com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse> getHeartbeatMethod() {
    io.grpc.MethodDescriptor<com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest, com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse> getHeartbeatMethod;
    if ((getHeartbeatMethod = RegistryServiceGrpc.getHeartbeatMethod) == null) {
      synchronized (RegistryServiceGrpc.class) {
        if ((getHeartbeatMethod = RegistryServiceGrpc.getHeartbeatMethod) == null) {
          RegistryServiceGrpc.getHeartbeatMethod = getHeartbeatMethod =
              io.grpc.MethodDescriptor.<com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest, com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse>newBuilder()
              .setType(io.grpc.MethodDescriptor.MethodType.UNARY)
              .setFullMethodName(generateFullMethodName(SERVICE_NAME, "Heartbeat"))
              .setSampledToLocalTracing(true)
              .setRequestMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest.getDefaultInstance()))
              .setResponseMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse.getDefaultInstance()))
              .setSchemaDescriptor(new RegistryServiceMethodDescriptorSupplier("Heartbeat"))
              .build();
        }
      }
    }
    return getHeartbeatMethod;
  }

  /**
   * Creates a new async stub that supports all call types for the service
   */
  public static RegistryServiceStub newStub(io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<RegistryServiceStub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<RegistryServiceStub>() {
        @java.lang.Override
        public RegistryServiceStub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new RegistryServiceStub(channel, callOptions);
        }
      };
    return RegistryServiceStub.newStub(factory, channel);
  }

  /**
   * Creates a new blocking-style stub that supports all types of calls on the service
   */
  public static RegistryServiceBlockingV2Stub newBlockingV2Stub(
      io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<RegistryServiceBlockingV2Stub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<RegistryServiceBlockingV2Stub>() {
        @java.lang.Override
        public RegistryServiceBlockingV2Stub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new RegistryServiceBlockingV2Stub(channel, callOptions);
        }
      };
    return RegistryServiceBlockingV2Stub.newStub(factory, channel);
  }

  /**
   * Creates a new blocking-style stub that supports unary and streaming output calls on the service
   */
  public static RegistryServiceBlockingStub newBlockingStub(
      io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<RegistryServiceBlockingStub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<RegistryServiceBlockingStub>() {
        @java.lang.Override
        public RegistryServiceBlockingStub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new RegistryServiceBlockingStub(channel, callOptions);
        }
      };
    return RegistryServiceBlockingStub.newStub(factory, channel);
  }

  /**
   * Creates a new ListenableFuture-style stub that supports unary calls on the service
   */
  public static RegistryServiceFutureStub newFutureStub(
      io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<RegistryServiceFutureStub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<RegistryServiceFutureStub>() {
        @java.lang.Override
        public RegistryServiceFutureStub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new RegistryServiceFutureStub(channel, callOptions);
        }
      };
    return RegistryServiceFutureStub.newStub(factory, channel);
  }

  /**
   * <pre>
   * RegistryService is served by the lobby and called by every game server and relay.
   * </pre>
   */
  public interface AsyncService {

    /**
     * <pre>
     * Heartbeat reports that a game server is alive, every 2 seconds. A server whose heartbeat
     * stops for 6 seconds is treated as dead, and its rooms are allocated elsewhere.
     * </pre>
     */
    default void heartbeat(com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest request,
        io.grpc.stub.StreamObserver<com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse> responseObserver) {
      io.grpc.stub.ServerCalls.asyncUnimplementedUnaryCall(getHeartbeatMethod(), responseObserver);
    }
  }

  /**
   * Base class for the server implementation of the service RegistryService.
   * <pre>
   * RegistryService is served by the lobby and called by every game server and relay.
   * </pre>
   */
  public static abstract class RegistryServiceImplBase
      implements io.grpc.BindableService, AsyncService {

    @java.lang.Override public final io.grpc.ServerServiceDefinition bindService() {
      return RegistryServiceGrpc.bindService(this);
    }
  }

  /**
   * A stub to allow clients to do asynchronous rpc calls to service RegistryService.
   * <pre>
   * RegistryService is served by the lobby and called by every game server and relay.
   * </pre>
   */
  public static final class RegistryServiceStub
      extends io.grpc.stub.AbstractAsyncStub<RegistryServiceStub> {
    private RegistryServiceStub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected RegistryServiceStub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new RegistryServiceStub(channel, callOptions);
    }

    /**
     * <pre>
     * Heartbeat reports that a game server is alive, every 2 seconds. A server whose heartbeat
     * stops for 6 seconds is treated as dead, and its rooms are allocated elsewhere.
     * </pre>
     */
    public void heartbeat(com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest request,
        io.grpc.stub.StreamObserver<com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse> responseObserver) {
      io.grpc.stub.ClientCalls.asyncUnaryCall(
          getChannel().newCall(getHeartbeatMethod(), getCallOptions()), request, responseObserver);
    }
  }

  /**
   * A stub to allow clients to do synchronous rpc calls to service RegistryService.
   * <pre>
   * RegistryService is served by the lobby and called by every game server and relay.
   * </pre>
   */
  public static final class RegistryServiceBlockingV2Stub
      extends io.grpc.stub.AbstractBlockingStub<RegistryServiceBlockingV2Stub> {
    private RegistryServiceBlockingV2Stub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected RegistryServiceBlockingV2Stub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new RegistryServiceBlockingV2Stub(channel, callOptions);
    }

    /**
     * <pre>
     * Heartbeat reports that a game server is alive, every 2 seconds. A server whose heartbeat
     * stops for 6 seconds is treated as dead, and its rooms are allocated elsewhere.
     * </pre>
     */
    public com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse heartbeat(com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest request) throws io.grpc.StatusException {
      return io.grpc.stub.ClientCalls.blockingV2UnaryCall(
          getChannel(), getHeartbeatMethod(), getCallOptions(), request);
    }
  }

  /**
   * A stub to allow clients to do limited synchronous rpc calls to service RegistryService.
   * <pre>
   * RegistryService is served by the lobby and called by every game server and relay.
   * </pre>
   */
  public static final class RegistryServiceBlockingStub
      extends io.grpc.stub.AbstractBlockingStub<RegistryServiceBlockingStub> {
    private RegistryServiceBlockingStub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected RegistryServiceBlockingStub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new RegistryServiceBlockingStub(channel, callOptions);
    }

    /**
     * <pre>
     * Heartbeat reports that a game server is alive, every 2 seconds. A server whose heartbeat
     * stops for 6 seconds is treated as dead, and its rooms are allocated elsewhere.
     * </pre>
     */
    public com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse heartbeat(com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest request) {
      return io.grpc.stub.ClientCalls.blockingUnaryCall(
          getChannel(), getHeartbeatMethod(), getCallOptions(), request);
    }
  }

  /**
   * A stub to allow clients to do ListenableFuture-style rpc calls to service RegistryService.
   * <pre>
   * RegistryService is served by the lobby and called by every game server and relay.
   * </pre>
   */
  public static final class RegistryServiceFutureStub
      extends io.grpc.stub.AbstractFutureStub<RegistryServiceFutureStub> {
    private RegistryServiceFutureStub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected RegistryServiceFutureStub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new RegistryServiceFutureStub(channel, callOptions);
    }

    /**
     * <pre>
     * Heartbeat reports that a game server is alive, every 2 seconds. A server whose heartbeat
     * stops for 6 seconds is treated as dead, and its rooms are allocated elsewhere.
     * </pre>
     */
    public com.google.common.util.concurrent.ListenableFuture<com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse> heartbeat(
        com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest request) {
      return io.grpc.stub.ClientCalls.futureUnaryCall(
          getChannel().newCall(getHeartbeatMethod(), getCallOptions()), request);
    }
  }

  private static final int METHODID_HEARTBEAT = 0;

  private static final class MethodHandlers<Req, Resp> implements
      io.grpc.stub.ServerCalls.UnaryMethod<Req, Resp>,
      io.grpc.stub.ServerCalls.ServerStreamingMethod<Req, Resp>,
      io.grpc.stub.ServerCalls.ClientStreamingMethod<Req, Resp>,
      io.grpc.stub.ServerCalls.BidiStreamingMethod<Req, Resp> {
    private final AsyncService serviceImpl;
    private final int methodId;

    MethodHandlers(AsyncService serviceImpl, int methodId) {
      this.serviceImpl = serviceImpl;
      this.methodId = methodId;
    }

    @java.lang.Override
    @java.lang.SuppressWarnings("unchecked")
    public void invoke(Req request, io.grpc.stub.StreamObserver<Resp> responseObserver) {
      switch (methodId) {
        case METHODID_HEARTBEAT:
          serviceImpl.heartbeat((com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest) request,
              (io.grpc.stub.StreamObserver<com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse>) responseObserver);
          break;
        default:
          throw new AssertionError();
      }
    }

    @java.lang.Override
    @java.lang.SuppressWarnings("unchecked")
    public io.grpc.stub.StreamObserver<Req> invoke(
        io.grpc.stub.StreamObserver<Resp> responseObserver) {
      switch (methodId) {
        default:
          throw new AssertionError();
      }
    }
  }

  public static final io.grpc.ServerServiceDefinition bindService(AsyncService service) {
    return io.grpc.ServerServiceDefinition.builder(getServiceDescriptor())
        .addMethod(
          getHeartbeatMethod(),
          io.grpc.stub.ServerCalls.asyncUnaryCall(
            new MethodHandlers<
              com.doodlearena.proto.doodle.lobby.v1.HeartbeatRequest,
              com.doodlearena.proto.doodle.lobby.v1.HeartbeatResponse>(
                service, METHODID_HEARTBEAT)))
        .build();
  }

  private static abstract class RegistryServiceBaseDescriptorSupplier
      implements io.grpc.protobuf.ProtoFileDescriptorSupplier, io.grpc.protobuf.ProtoServiceDescriptorSupplier {
    RegistryServiceBaseDescriptorSupplier() {}

    @java.lang.Override
    public com.google.protobuf.Descriptors.FileDescriptor getFileDescriptor() {
      return com.doodlearena.proto.doodle.lobby.v1.RegistryProto.getDescriptor();
    }

    @java.lang.Override
    public com.google.protobuf.Descriptors.ServiceDescriptor getServiceDescriptor() {
      return getFileDescriptor().findServiceByName("RegistryService");
    }
  }

  private static final class RegistryServiceFileDescriptorSupplier
      extends RegistryServiceBaseDescriptorSupplier {
    RegistryServiceFileDescriptorSupplier() {}
  }

  private static final class RegistryServiceMethodDescriptorSupplier
      extends RegistryServiceBaseDescriptorSupplier
      implements io.grpc.protobuf.ProtoMethodDescriptorSupplier {
    private final java.lang.String methodName;

    RegistryServiceMethodDescriptorSupplier(java.lang.String methodName) {
      this.methodName = methodName;
    }

    @java.lang.Override
    public com.google.protobuf.Descriptors.MethodDescriptor getMethodDescriptor() {
      return getServiceDescriptor().findMethodByName(methodName);
    }
  }

  private static volatile io.grpc.ServiceDescriptor serviceDescriptor;

  public static io.grpc.ServiceDescriptor getServiceDescriptor() {
    io.grpc.ServiceDescriptor result = serviceDescriptor;
    if (result == null) {
      synchronized (RegistryServiceGrpc.class) {
        result = serviceDescriptor;
        if (result == null) {
          serviceDescriptor = result = io.grpc.ServiceDescriptor.newBuilder(SERVICE_NAME)
              .setSchemaDescriptor(new RegistryServiceFileDescriptorSupplier())
              .addMethod(getHeartbeatMethod())
              .build();
        }
      }
    }
    return result;
  }
}
