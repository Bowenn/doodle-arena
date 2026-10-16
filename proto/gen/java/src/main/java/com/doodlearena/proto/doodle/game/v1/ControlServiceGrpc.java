package com.doodlearena.proto.doodle.game.v1;

import static io.grpc.MethodDescriptor.generateFullMethodName;

/**
 * <pre>
 * ControlService is served by every game server and called by the lobby. Every call has a
 * deadline.
 * </pre>
 */
@io.grpc.stub.annotations.GrpcGenerated
public final class ControlServiceGrpc {

  private ControlServiceGrpc() {}

  public static final java.lang.String SERVICE_NAME = "doodle.game.v1.ControlService";

  // Static method descriptors that strictly reflect the proto.
  private static volatile io.grpc.MethodDescriptor<com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest,
      com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse> getAllocateRoomMethod;

  @io.grpc.stub.annotations.RpcMethod(
      fullMethodName = SERVICE_NAME + '/' + "AllocateRoom",
      requestType = com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest.class,
      responseType = com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse.class,
      methodType = io.grpc.MethodDescriptor.MethodType.UNARY)
  public static io.grpc.MethodDescriptor<com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest,
      com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse> getAllocateRoomMethod() {
    io.grpc.MethodDescriptor<com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest, com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse> getAllocateRoomMethod;
    if ((getAllocateRoomMethod = ControlServiceGrpc.getAllocateRoomMethod) == null) {
      synchronized (ControlServiceGrpc.class) {
        if ((getAllocateRoomMethod = ControlServiceGrpc.getAllocateRoomMethod) == null) {
          ControlServiceGrpc.getAllocateRoomMethod = getAllocateRoomMethod =
              io.grpc.MethodDescriptor.<com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest, com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse>newBuilder()
              .setType(io.grpc.MethodDescriptor.MethodType.UNARY)
              .setFullMethodName(generateFullMethodName(SERVICE_NAME, "AllocateRoom"))
              .setSampledToLocalTracing(true)
              .setRequestMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest.getDefaultInstance()))
              .setResponseMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse.getDefaultInstance()))
              .setSchemaDescriptor(new ControlServiceMethodDescriptorSupplier("AllocateRoom"))
              .build();
        }
      }
    }
    return getAllocateRoomMethod;
  }

  private static volatile io.grpc.MethodDescriptor<com.doodlearena.proto.doodle.game.v1.DrainServerRequest,
      com.doodlearena.proto.doodle.game.v1.DrainServerResponse> getDrainServerMethod;

  @io.grpc.stub.annotations.RpcMethod(
      fullMethodName = SERVICE_NAME + '/' + "DrainServer",
      requestType = com.doodlearena.proto.doodle.game.v1.DrainServerRequest.class,
      responseType = com.doodlearena.proto.doodle.game.v1.DrainServerResponse.class,
      methodType = io.grpc.MethodDescriptor.MethodType.UNARY)
  public static io.grpc.MethodDescriptor<com.doodlearena.proto.doodle.game.v1.DrainServerRequest,
      com.doodlearena.proto.doodle.game.v1.DrainServerResponse> getDrainServerMethod() {
    io.grpc.MethodDescriptor<com.doodlearena.proto.doodle.game.v1.DrainServerRequest, com.doodlearena.proto.doodle.game.v1.DrainServerResponse> getDrainServerMethod;
    if ((getDrainServerMethod = ControlServiceGrpc.getDrainServerMethod) == null) {
      synchronized (ControlServiceGrpc.class) {
        if ((getDrainServerMethod = ControlServiceGrpc.getDrainServerMethod) == null) {
          ControlServiceGrpc.getDrainServerMethod = getDrainServerMethod =
              io.grpc.MethodDescriptor.<com.doodlearena.proto.doodle.game.v1.DrainServerRequest, com.doodlearena.proto.doodle.game.v1.DrainServerResponse>newBuilder()
              .setType(io.grpc.MethodDescriptor.MethodType.UNARY)
              .setFullMethodName(generateFullMethodName(SERVICE_NAME, "DrainServer"))
              .setSampledToLocalTracing(true)
              .setRequestMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  com.doodlearena.proto.doodle.game.v1.DrainServerRequest.getDefaultInstance()))
              .setResponseMarshaller(io.grpc.protobuf.ProtoUtils.marshaller(
                  com.doodlearena.proto.doodle.game.v1.DrainServerResponse.getDefaultInstance()))
              .setSchemaDescriptor(new ControlServiceMethodDescriptorSupplier("DrainServer"))
              .build();
        }
      }
    }
    return getDrainServerMethod;
  }

  /**
   * Creates a new async stub that supports all call types for the service
   */
  public static ControlServiceStub newStub(io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<ControlServiceStub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<ControlServiceStub>() {
        @java.lang.Override
        public ControlServiceStub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new ControlServiceStub(channel, callOptions);
        }
      };
    return ControlServiceStub.newStub(factory, channel);
  }

  /**
   * Creates a new blocking-style stub that supports all types of calls on the service
   */
  public static ControlServiceBlockingV2Stub newBlockingV2Stub(
      io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<ControlServiceBlockingV2Stub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<ControlServiceBlockingV2Stub>() {
        @java.lang.Override
        public ControlServiceBlockingV2Stub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new ControlServiceBlockingV2Stub(channel, callOptions);
        }
      };
    return ControlServiceBlockingV2Stub.newStub(factory, channel);
  }

  /**
   * Creates a new blocking-style stub that supports unary and streaming output calls on the service
   */
  public static ControlServiceBlockingStub newBlockingStub(
      io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<ControlServiceBlockingStub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<ControlServiceBlockingStub>() {
        @java.lang.Override
        public ControlServiceBlockingStub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new ControlServiceBlockingStub(channel, callOptions);
        }
      };
    return ControlServiceBlockingStub.newStub(factory, channel);
  }

  /**
   * Creates a new ListenableFuture-style stub that supports unary calls on the service
   */
  public static ControlServiceFutureStub newFutureStub(
      io.grpc.Channel channel) {
    io.grpc.stub.AbstractStub.StubFactory<ControlServiceFutureStub> factory =
      new io.grpc.stub.AbstractStub.StubFactory<ControlServiceFutureStub>() {
        @java.lang.Override
        public ControlServiceFutureStub newStub(io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
          return new ControlServiceFutureStub(channel, callOptions);
        }
      };
    return ControlServiceFutureStub.newStub(factory, channel);
  }

  /**
   * <pre>
   * ControlService is served by every game server and called by the lobby. Every call has a
   * deadline.
   * </pre>
   */
  public interface AsyncService {

    /**
     * <pre>
     * AllocateRoom creates a room actor on this server and takes the room's lease.
     * Idempotent for the same room_id: the lobby retries on timeouts, and failover allocates a
     * dead server's rooms again. Errors: RESOURCE_EXHAUSTED when the server is full,
     * FAILED_PRECONDITION while it drains.
     * </pre>
     */
    default void allocateRoom(com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest request,
        io.grpc.stub.StreamObserver<com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse> responseObserver) {
      io.grpc.stub.ServerCalls.asyncUnimplementedUnaryCall(getAllocateRoomMethod(), responseObserver);
    }

    /**
     * <pre>
     * DrainServer stops this server from accepting new rooms before a deploy. Existing rooms
     * finish their rounds or are handed off; the server shuts down when none are left.
     * </pre>
     */
    default void drainServer(com.doodlearena.proto.doodle.game.v1.DrainServerRequest request,
        io.grpc.stub.StreamObserver<com.doodlearena.proto.doodle.game.v1.DrainServerResponse> responseObserver) {
      io.grpc.stub.ServerCalls.asyncUnimplementedUnaryCall(getDrainServerMethod(), responseObserver);
    }
  }

  /**
   * Base class for the server implementation of the service ControlService.
   * <pre>
   * ControlService is served by every game server and called by the lobby. Every call has a
   * deadline.
   * </pre>
   */
  public static abstract class ControlServiceImplBase
      implements io.grpc.BindableService, AsyncService {

    @java.lang.Override public final io.grpc.ServerServiceDefinition bindService() {
      return ControlServiceGrpc.bindService(this);
    }
  }

  /**
   * A stub to allow clients to do asynchronous rpc calls to service ControlService.
   * <pre>
   * ControlService is served by every game server and called by the lobby. Every call has a
   * deadline.
   * </pre>
   */
  public static final class ControlServiceStub
      extends io.grpc.stub.AbstractAsyncStub<ControlServiceStub> {
    private ControlServiceStub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected ControlServiceStub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new ControlServiceStub(channel, callOptions);
    }

    /**
     * <pre>
     * AllocateRoom creates a room actor on this server and takes the room's lease.
     * Idempotent for the same room_id: the lobby retries on timeouts, and failover allocates a
     * dead server's rooms again. Errors: RESOURCE_EXHAUSTED when the server is full,
     * FAILED_PRECONDITION while it drains.
     * </pre>
     */
    public void allocateRoom(com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest request,
        io.grpc.stub.StreamObserver<com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse> responseObserver) {
      io.grpc.stub.ClientCalls.asyncUnaryCall(
          getChannel().newCall(getAllocateRoomMethod(), getCallOptions()), request, responseObserver);
    }

    /**
     * <pre>
     * DrainServer stops this server from accepting new rooms before a deploy. Existing rooms
     * finish their rounds or are handed off; the server shuts down when none are left.
     * </pre>
     */
    public void drainServer(com.doodlearena.proto.doodle.game.v1.DrainServerRequest request,
        io.grpc.stub.StreamObserver<com.doodlearena.proto.doodle.game.v1.DrainServerResponse> responseObserver) {
      io.grpc.stub.ClientCalls.asyncUnaryCall(
          getChannel().newCall(getDrainServerMethod(), getCallOptions()), request, responseObserver);
    }
  }

  /**
   * A stub to allow clients to do synchronous rpc calls to service ControlService.
   * <pre>
   * ControlService is served by every game server and called by the lobby. Every call has a
   * deadline.
   * </pre>
   */
  public static final class ControlServiceBlockingV2Stub
      extends io.grpc.stub.AbstractBlockingStub<ControlServiceBlockingV2Stub> {
    private ControlServiceBlockingV2Stub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected ControlServiceBlockingV2Stub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new ControlServiceBlockingV2Stub(channel, callOptions);
    }

    /**
     * <pre>
     * AllocateRoom creates a room actor on this server and takes the room's lease.
     * Idempotent for the same room_id: the lobby retries on timeouts, and failover allocates a
     * dead server's rooms again. Errors: RESOURCE_EXHAUSTED when the server is full,
     * FAILED_PRECONDITION while it drains.
     * </pre>
     */
    public com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse allocateRoom(com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest request) throws io.grpc.StatusException {
      return io.grpc.stub.ClientCalls.blockingV2UnaryCall(
          getChannel(), getAllocateRoomMethod(), getCallOptions(), request);
    }

    /**
     * <pre>
     * DrainServer stops this server from accepting new rooms before a deploy. Existing rooms
     * finish their rounds or are handed off; the server shuts down when none are left.
     * </pre>
     */
    public com.doodlearena.proto.doodle.game.v1.DrainServerResponse drainServer(com.doodlearena.proto.doodle.game.v1.DrainServerRequest request) throws io.grpc.StatusException {
      return io.grpc.stub.ClientCalls.blockingV2UnaryCall(
          getChannel(), getDrainServerMethod(), getCallOptions(), request);
    }
  }

  /**
   * A stub to allow clients to do limited synchronous rpc calls to service ControlService.
   * <pre>
   * ControlService is served by every game server and called by the lobby. Every call has a
   * deadline.
   * </pre>
   */
  public static final class ControlServiceBlockingStub
      extends io.grpc.stub.AbstractBlockingStub<ControlServiceBlockingStub> {
    private ControlServiceBlockingStub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected ControlServiceBlockingStub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new ControlServiceBlockingStub(channel, callOptions);
    }

    /**
     * <pre>
     * AllocateRoom creates a room actor on this server and takes the room's lease.
     * Idempotent for the same room_id: the lobby retries on timeouts, and failover allocates a
     * dead server's rooms again. Errors: RESOURCE_EXHAUSTED when the server is full,
     * FAILED_PRECONDITION while it drains.
     * </pre>
     */
    public com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse allocateRoom(com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest request) {
      return io.grpc.stub.ClientCalls.blockingUnaryCall(
          getChannel(), getAllocateRoomMethod(), getCallOptions(), request);
    }

    /**
     * <pre>
     * DrainServer stops this server from accepting new rooms before a deploy. Existing rooms
     * finish their rounds or are handed off; the server shuts down when none are left.
     * </pre>
     */
    public com.doodlearena.proto.doodle.game.v1.DrainServerResponse drainServer(com.doodlearena.proto.doodle.game.v1.DrainServerRequest request) {
      return io.grpc.stub.ClientCalls.blockingUnaryCall(
          getChannel(), getDrainServerMethod(), getCallOptions(), request);
    }
  }

  /**
   * A stub to allow clients to do ListenableFuture-style rpc calls to service ControlService.
   * <pre>
   * ControlService is served by every game server and called by the lobby. Every call has a
   * deadline.
   * </pre>
   */
  public static final class ControlServiceFutureStub
      extends io.grpc.stub.AbstractFutureStub<ControlServiceFutureStub> {
    private ControlServiceFutureStub(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      super(channel, callOptions);
    }

    @java.lang.Override
    protected ControlServiceFutureStub build(
        io.grpc.Channel channel, io.grpc.CallOptions callOptions) {
      return new ControlServiceFutureStub(channel, callOptions);
    }

    /**
     * <pre>
     * AllocateRoom creates a room actor on this server and takes the room's lease.
     * Idempotent for the same room_id: the lobby retries on timeouts, and failover allocates a
     * dead server's rooms again. Errors: RESOURCE_EXHAUSTED when the server is full,
     * FAILED_PRECONDITION while it drains.
     * </pre>
     */
    public com.google.common.util.concurrent.ListenableFuture<com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse> allocateRoom(
        com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest request) {
      return io.grpc.stub.ClientCalls.futureUnaryCall(
          getChannel().newCall(getAllocateRoomMethod(), getCallOptions()), request);
    }

    /**
     * <pre>
     * DrainServer stops this server from accepting new rooms before a deploy. Existing rooms
     * finish their rounds or are handed off; the server shuts down when none are left.
     * </pre>
     */
    public com.google.common.util.concurrent.ListenableFuture<com.doodlearena.proto.doodle.game.v1.DrainServerResponse> drainServer(
        com.doodlearena.proto.doodle.game.v1.DrainServerRequest request) {
      return io.grpc.stub.ClientCalls.futureUnaryCall(
          getChannel().newCall(getDrainServerMethod(), getCallOptions()), request);
    }
  }

  private static final int METHODID_ALLOCATE_ROOM = 0;
  private static final int METHODID_DRAIN_SERVER = 1;

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
        case METHODID_ALLOCATE_ROOM:
          serviceImpl.allocateRoom((com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest) request,
              (io.grpc.stub.StreamObserver<com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse>) responseObserver);
          break;
        case METHODID_DRAIN_SERVER:
          serviceImpl.drainServer((com.doodlearena.proto.doodle.game.v1.DrainServerRequest) request,
              (io.grpc.stub.StreamObserver<com.doodlearena.proto.doodle.game.v1.DrainServerResponse>) responseObserver);
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
          getAllocateRoomMethod(),
          io.grpc.stub.ServerCalls.asyncUnaryCall(
            new MethodHandlers<
              com.doodlearena.proto.doodle.game.v1.AllocateRoomRequest,
              com.doodlearena.proto.doodle.game.v1.AllocateRoomResponse>(
                service, METHODID_ALLOCATE_ROOM)))
        .addMethod(
          getDrainServerMethod(),
          io.grpc.stub.ServerCalls.asyncUnaryCall(
            new MethodHandlers<
              com.doodlearena.proto.doodle.game.v1.DrainServerRequest,
              com.doodlearena.proto.doodle.game.v1.DrainServerResponse>(
                service, METHODID_DRAIN_SERVER)))
        .build();
  }

  private static abstract class ControlServiceBaseDescriptorSupplier
      implements io.grpc.protobuf.ProtoFileDescriptorSupplier, io.grpc.protobuf.ProtoServiceDescriptorSupplier {
    ControlServiceBaseDescriptorSupplier() {}

    @java.lang.Override
    public com.google.protobuf.Descriptors.FileDescriptor getFileDescriptor() {
      return com.doodlearena.proto.doodle.game.v1.ControlProto.getDescriptor();
    }

    @java.lang.Override
    public com.google.protobuf.Descriptors.ServiceDescriptor getServiceDescriptor() {
      return getFileDescriptor().findServiceByName("ControlService");
    }
  }

  private static final class ControlServiceFileDescriptorSupplier
      extends ControlServiceBaseDescriptorSupplier {
    ControlServiceFileDescriptorSupplier() {}
  }

  private static final class ControlServiceMethodDescriptorSupplier
      extends ControlServiceBaseDescriptorSupplier
      implements io.grpc.protobuf.ProtoMethodDescriptorSupplier {
    private final java.lang.String methodName;

    ControlServiceMethodDescriptorSupplier(java.lang.String methodName) {
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
      synchronized (ControlServiceGrpc.class) {
        result = serviceDescriptor;
        if (result == null) {
          serviceDescriptor = result = io.grpc.ServiceDescriptor.newBuilder(SERVICE_NAME)
              .setSchemaDescriptor(new ControlServiceFileDescriptorSupplier())
              .addMethod(getAllocateRoomMethod())
              .addMethod(getDrainServerMethod())
              .build();
        }
      }
    }
    return result;
  }
}
