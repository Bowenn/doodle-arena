# The -otel variant ships ngx_otel_module, which task 1.5 turns on for tracing.
FROM nginx:1.30.6-otel
RUN rm -f /etc/nginx/conf.d/default.conf
COPY nginx.conf /etc/nginx/nginx.conf
COPY proxy_common.conf websocket.conf identity.js /etc/nginx/
# Fail the build on a config error instead of at container start.
RUN nginx -t
EXPOSE 80
