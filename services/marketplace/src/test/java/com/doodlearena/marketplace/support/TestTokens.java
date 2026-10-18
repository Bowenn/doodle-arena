package com.doodlearena.marketplace.support;

import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.time.Instant;
import java.util.Date;
import java.util.List;
import java.util.UUID;
import java.util.function.Consumer;

import com.nimbusds.jose.JOSEException;
import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jose.crypto.MACSigner;
import com.nimbusds.jose.crypto.RSASSASigner;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jose.jwk.RSAKey;
import com.nimbusds.jose.jwk.gen.RSAKeyGenerator;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.PlainJWT;
import com.nimbusds.jwt.SignedJWT;
import com.sun.net.httpserver.HttpServer;

/**
 * Plays auth for tests: an RSA key served as a JWKS over HTTP (the decoder fetches it like the
 * real one) and access tokens shaped like auth's (app/tokens.py), plus broken variants.
 */
public final class TestTokens {

	public static final String ISSUER = "http://localhost:8080/api/auth";

	public static final String AUDIENCE = "doodle-api";

	private static final RSAKey KEY = generate("test-key");

	private static final RSAKey FOREIGN_KEY = generate("test-key");

	private static final HttpServer JWKS = serve();

	private TestTokens() {
	}

	public static String jwksUrl() {
		return "http://127.0.0.1:" + JWKS.getAddress().getPort() + "/jwks.json";
	}

	public static String valid(String userId) {
		return sign(KEY, claims(userId, (c) -> {
		}));
	}

	public static String with(String userId, Consumer<JWTClaimsSet.Builder> change) {
		return sign(KEY, claims(userId, change));
	}

	/** Signed with a key that isn't in the JWKS, but with the same key id. */
	public static String foreignKey(String userId) {
		return sign(FOREIGN_KEY, claims(userId, (c) -> {
		}));
	}

	public static String hs256(String userId) {
		try {
			SignedJWT jwt = new SignedJWT(new JWSHeader(JWSAlgorithm.HS256), claims(userId, (c) -> {
			}));
			jwt.sign(new MACSigner(KEY.toRSAPublicKey().getEncoded()));
			return jwt.serialize();
		}
		catch (JOSEException ex) {
			throw new IllegalStateException(ex);
		}
	}

	public static String unsigned(String userId) {
		return new PlainJWT(claims(userId, (c) -> {
		})).serialize();
	}

	private static JWTClaimsSet claims(String userId, Consumer<JWTClaimsSet.Builder> change) {
		Instant now = Instant.now();
		JWTClaimsSet.Builder builder = new JWTClaimsSet.Builder().subject(userId)
			.issuer(ISSUER)
			.audience(AUDIENCE)
			.issueTime(Date.from(now))
			.expirationTime(Date.from(now.plus(Duration.ofMinutes(15))))
			.jwtID(UUID.randomUUID().toString())
			.claim("roles", List.of("player"));
		change.accept(builder);
		return builder.build();
	}

	private static String sign(RSAKey key, JWTClaimsSet claims) {
		try {
			SignedJWT jwt = new SignedJWT(new JWSHeader.Builder(JWSAlgorithm.RS256).keyID(key.getKeyID()).build(), claims);
			jwt.sign(new RSASSASigner(key));
			return jwt.serialize();
		}
		catch (JOSEException ex) {
			throw new IllegalStateException(ex);
		}
	}

	private static RSAKey generate(String keyId) {
		try {
			return new RSAKeyGenerator(2048).keyID(keyId).generate();
		}
		catch (JOSEException ex) {
			throw new IllegalStateException(ex);
		}
	}

	private static HttpServer serve() {
		try {
			HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
			byte[] body = new JWKSet(KEY.toPublicJWK()).toString().getBytes(StandardCharsets.UTF_8);
			server.createContext("/jwks.json", (exchange) -> {
				exchange.getResponseHeaders().add("Content-Type", "application/json");
				exchange.sendResponseHeaders(200, body.length);
				try (OutputStream out = exchange.getResponseBody()) {
					out.write(body);
				}
			});
			server.start();
			return server;
		}
		catch (IOException ex) {
			throw new IllegalStateException(ex);
		}
	}

}
