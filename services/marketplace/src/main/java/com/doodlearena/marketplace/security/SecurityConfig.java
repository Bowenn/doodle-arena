package com.doodlearena.marketplace.security;

import java.io.IOException;
import java.time.Duration;
import java.util.List;
import java.util.Objects;

import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import tools.jackson.databind.json.JsonMapper;

import org.springframework.beans.factory.annotation.Value;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.http.HttpMethod;
import org.springframework.http.MediaType;
import org.springframework.security.config.annotation.web.builders.HttpSecurity;
import org.springframework.security.config.http.SessionCreationPolicy;
import org.springframework.security.oauth2.core.DelegatingOAuth2TokenValidator;
import org.springframework.security.oauth2.core.OAuth2TokenValidator;
import org.springframework.security.oauth2.jose.jws.SignatureAlgorithm;
import org.springframework.security.oauth2.jwt.Jwt;
import org.springframework.security.oauth2.jwt.JwtClaimNames;
import org.springframework.security.oauth2.jwt.JwtClaimValidator;
import org.springframework.security.oauth2.jwt.JwtDecoder;
import org.springframework.security.oauth2.jwt.JwtIssuerValidator;
import org.springframework.security.oauth2.jwt.JwtTimestampValidator;
import org.springframework.security.oauth2.jwt.NimbusJwtDecoder;
import org.springframework.security.oauth2.server.resource.authentication.JwtAuthenticationConverter;
import org.springframework.security.oauth2.server.resource.authentication.JwtGrantedAuthoritiesConverter;
import org.springframework.security.web.AuthenticationEntryPoint;
import org.springframework.security.web.SecurityFilterChain;

import com.doodlearena.marketplace.api.ErrorBody;

/**
 * Access tokens are verified here, not at the gateway (ADR 0001): RS256 only, signed by a key in
 * auth's JWKS (fetched on demand, cached, refetched for an unknown key id), issuer, audience
 * {@code doodle-api}, and {@code exp}/{@code iat} with at most 30 seconds of clock skew. The API
 * takes the token from the Authorization header only, never a cookie, so it needs no CSRF token.
 */
@Configuration(proxyBeanMethods = false)
public class SecurityConfig {

	private static final Duration CLOCK_SKEW = Duration.ofSeconds(30);

	@Bean
	JwtDecoder jwtDecoder(@Value("${app.jwt.jwks-url}") String jwksUrl, @Value("${app.jwt.issuer}") String issuer,
			@Value("${app.jwt.audience}") String audience) {
		NimbusJwtDecoder decoder = NimbusJwtDecoder.withJwkSetUri(jwksUrl).jwsAlgorithm(SignatureAlgorithm.RS256).build();
		decoder.setJwtValidator(validator(issuer, audience));
		return decoder;
	}

	static OAuth2TokenValidator<Jwt> validator(String issuer, String audience) {
		return new DelegatingOAuth2TokenValidator<>(List.of(
				new JwtTimestampValidator(CLOCK_SKEW),
				// The timestamp validator skips claims that are absent; ADR 0001 requires them.
				new JwtClaimValidator<Object>(JwtClaimNames.EXP, Objects::nonNull),
				new JwtClaimValidator<Object>(JwtClaimNames.IAT, Objects::nonNull),
				new JwtClaimValidator<Object>(JwtClaimNames.SUB, Objects::nonNull),
				new JwtIssuerValidator(issuer),
				new JwtClaimValidator<List<String>>(JwtClaimNames.AUD, (aud) -> aud != null && aud.contains(audience))));
	}

	@Bean
	SecurityFilterChain api(HttpSecurity http, JsonMapper json) throws Exception {
		AuthenticationEntryPoint unauthenticated = (request, response, ex) -> write(response, request, json,
				HttpServletResponse.SC_UNAUTHORIZED, "UNAUTHENTICATED", "a valid access token is required");
		http.authorizeHttpRequests((requests) -> requests
				// Stripe signs its webhook instead (StripeWebhookController).
				.requestMatchers("/healthz", "/api/market/stripe/webhook")
				.permitAll()
				// Only an admin creates a limited drop (ADR 0022); grant it with scripts/grant-role.sh.
				.requestMatchers(HttpMethod.POST, "/api/market/drops")
				.hasRole("admin")
				.anyRequest()
				.authenticated())
			.sessionManagement((sessions) -> sessions.sessionCreationPolicy(SessionCreationPolicy.STATELESS))
			.csrf((csrf) -> csrf.disable())
			.oauth2ResourceServer((server) -> server.jwt((jwt) -> jwt.jwtAuthenticationConverter(roles()))
				.authenticationEntryPoint(unauthenticated))
			.exceptionHandling((errors) -> errors.authenticationEntryPoint(unauthenticated)
				.accessDeniedHandler((request, response, ex) -> write(response, request, json,
						HttpServletResponse.SC_FORBIDDEN, "FORBIDDEN", "not allowed")));
		return http.build();
	}

	/** The token's {@code roles} claim (player, moderator, admin) as ROLE_ authorities. */
	static JwtAuthenticationConverter roles() {
		JwtGrantedAuthoritiesConverter authorities = new JwtGrantedAuthoritiesConverter();
		authorities.setAuthoritiesClaimName("roles");
		authorities.setAuthorityPrefix("ROLE_");
		JwtAuthenticationConverter converter = new JwtAuthenticationConverter();
		converter.setJwtGrantedAuthoritiesConverter(authorities);
		return converter;
	}

	private static void write(HttpServletResponse response, HttpServletRequest request, JsonMapper json, int status,
			String code, String message) throws IOException {
		response.setStatus(status);
		response.setContentType(MediaType.APPLICATION_JSON_VALUE);
		json.writeValue(response.getOutputStream(), ErrorBody.of(code, message, request));
	}

}
