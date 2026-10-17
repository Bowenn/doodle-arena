# The -otel variant ships ngx_otel_module, which task 1.5 turns on for tracing.
FROM nginx:1.30.6-otel
RUN rm -f /etc/nginx/conf.d/default.conf
# nginx.conf is rendered at start from the template, with this instance's identity
# (docs/observability.md); the defaults are for a container started without them.
ENV NGINX_ENVSUBST_OUTPUT_DIR=/etc/nginx NGINX_ENVSUBST_FILTER=^SERVICE_ \
    SERVICE_VERSION=0.1.0 SERVICE_INSTANCE_ID=gateway
COPY nginx.conf.template /etc/nginx/templates/
COPY proxy_common.conf websocket.conf identity.js /etc/nginx/
# Fail the build on a config error instead of at container start.
RUN envsubst '${SERVICE_VERSION} ${SERVICE_INSTANCE_ID}' < /etc/nginx/templates/nginx.conf.template \
        > /etc/nginx/nginx.conf \
    && nginx -t
EXPOSE 80
