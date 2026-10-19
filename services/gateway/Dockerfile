# The -otel variant ships ngx_otel_module, which task 1.5 turns on for tracing.
FROM nginx:1.30.6-otel
RUN rm -f /etc/nginx/conf.d/default.conf
# nginx.conf is rendered at start from the template, with this instance's identity
# (docs/observability.md); the defaults are for a container started without them.
# SERVICE_TRUSTED_PROXIES: who may set X-Forwarded-For (nobody locally; the VPC in AWS).
ENV NGINX_ENVSUBST_OUTPUT_DIR=/etc/nginx NGINX_ENVSUBST_FILTER=^SERVICE_ \
    SERVICE_VERSION=0.1.0 SERVICE_INSTANCE_ID=gateway \
    SERVICE_OTLP_ENDPOINT=otel-lgtm:4317 SERVICE_ENVIRONMENT=local SERVICE_RESOLVER=127.0.0.11 \
    SERVICE_DOMAIN= SERVICE_WEB_PORT=5173 SERVICE_TRUSTED_PROXIES=127.0.0.1/32
COPY nginx.conf.template /etc/nginx/templates/
COPY proxy_common.conf websocket.conf identity.js /etc/nginx/
# Fail the build on a config error instead of at container start, for the AWS values too
# (the local ones are rendered last and stay as the image's default config).
RUN vars='${SERVICE_VERSION} ${SERVICE_INSTANCE_ID} ${SERVICE_OTLP_ENDPOINT} ${SERVICE_ENVIRONMENT} ${SERVICE_RESOLVER} ${SERVICE_DOMAIN} ${SERVICE_WEB_PORT} ${SERVICE_TRUSTED_PROXIES}' \
    && SERVICE_OTLP_ENDPOINT=data.doodle.internal:4317 SERVICE_ENVIRONMENT=aws SERVICE_RESOLVER=169.254.169.253 \
       SERVICE_DOMAIN=.doodle.internal SERVICE_WEB_PORT=8080 SERVICE_TRUSTED_PROXIES=10.0.0.0/16 \
       envsubst "$vars" < /etc/nginx/templates/nginx.conf.template > /etc/nginx/nginx.conf \
    && nginx -t \
    && envsubst "$vars" < /etc/nginx/templates/nginx.conf.template > /etc/nginx/nginx.conf \
    && nginx -t
EXPOSE 80
