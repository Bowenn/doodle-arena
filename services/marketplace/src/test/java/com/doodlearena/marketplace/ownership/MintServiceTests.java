package com.doodlearena.marketplace.ownership;

import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Queue;
import java.util.UUID;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

import org.junit.jupiter.api.Test;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.dao.DataAccessException;
import org.springframework.jdbc.core.simple.JdbcClient;

import com.doodlearena.marketplace.ownership.MintService.MintResult;
import com.doodlearena.marketplace.support.IntegrationTest;

import static com.doodlearena.marketplace.support.TestEvents.approved;
import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

@IntegrationTest
class MintServiceTests {

	@Autowired
	private MintService mints;

	@Autowired
	private JdbcClient db;

	@Test
	void mintRecordsOwnerAuthorsAndOneMintEvent() {
		UUID drawing = UUID.randomUUID();

		MintResult result = this.mints.mint(MintRequest.from(approved(drawing, "u_ada", "u_bob")));

		assertThat(result).isEqualTo(MintResult.MINTED);
		Map<String, Object> ownership = this.db.sql("SELECT owner_id, version FROM ownership WHERE drawing_id = ?")
			.param(drawing)
			.query()
			.singleRow();
		assertThat(ownership).containsEntry("owner_id", "u_ada").containsEntry("version", 1L);
		assertThat(this.db.sql("SELECT user_id FROM drawing_authors WHERE drawing_id = ? ORDER BY author_order")
			.param(drawing)
			.query(String.class)
			.list()).containsExactly("u_ada", "u_bob");
		Map<String, Object> event = this.db
			.sql("SELECT kind, from_owner, to_owner, sale_id, source_event_id FROM ownership_events WHERE drawing_id = ?")
			.param(drawing)
			.query()
			.singleRow();
		assertThat(event).containsEntry("kind", "MINT")
			.containsEntry("from_owner", null)
			.containsEntry("to_owner", "u_ada")
			.containsEntry("sale_id", null)
			.containsEntry("source_event_id", "evt-" + drawing);
	}

	@Test
	void redeliveryIsADuplicateAndChangesNothing() {
		UUID drawing = UUID.randomUUID();
		MintRequest request = MintRequest.from(approved(drawing, "u_cyd"));

		assertThat(this.mints.mint(request)).isEqualTo(MintResult.MINTED);
		assertThat(this.mints.mint(request)).isEqualTo(MintResult.DUPLICATE);

		assertThat(eventsOf(drawing)).isEqualTo(1);
		assertThat(this.db.sql("SELECT COUNT(*) FROM drawing_authors WHERE drawing_id = ?").param(drawing).query(Long.class).single())
			.isEqualTo(1);
	}

	@Test
	void concurrentDeliveriesMintOnce() throws Exception {
		UUID drawing = UUID.randomUUID();
		MintRequest request = MintRequest.from(approved(drawing, "u_dee", "u_eve"));
		Queue<MintResult> results = new ConcurrentLinkedQueue<>();
		CountDownLatch start = new CountDownLatch(1);
		try (ExecutorService pool = Executors.newFixedThreadPool(10)) {
			List<Future<?>> futures = new ArrayList<>();
			for (int i = 0; i < 10; i++) {
				futures.add(pool.submit(() -> {
					start.await();
					results.add(this.mints.mint(request));
					return null;
				}));
			}
			start.countDown();
			for (Future<?> future : futures) {
				future.get(1, TimeUnit.MINUTES);
			}
		}

		assertThat(results).containsOnlyOnce(MintResult.MINTED).hasSize(10);
		assertThat(eventsOf(drawing)).isEqualTo(1);
	}

	@Test
	void eventsBreakingTheContractAreRefused() {
		UUID drawing = UUID.randomUUID();
		assertThatThrownBy(() -> MintRequest.from(approved(drawing).toBuilder().clearAuthorIds().build()))
			.isInstanceOf(InvalidDrawingEventException.class);
		assertThatThrownBy(() -> MintRequest.from(approved(drawing, "u_a", "u_b").toBuilder().setInitialOwnerId("u_b").build()))
			.isInstanceOf(InvalidDrawingEventException.class);
		assertThatThrownBy(() -> MintRequest.from(approved(drawing, "u_a").toBuilder().setDrawingId("d_42").build()))
			.isInstanceOf(InvalidDrawingEventException.class);
		assertThatThrownBy(() -> MintRequest.from(approved(drawing, "u_a", "u_a")))
			.isInstanceOf(InvalidDrawingEventException.class);
		assertThatThrownBy(() -> MintRequest.from(approved(drawing, "u_a", " ")))
			.isInstanceOf(InvalidDrawingEventException.class);
	}

	@Test
	void provenanceIsAppendOnlyWithOneMint() {
		UUID drawing = UUID.randomUUID();
		this.mints.mint(MintRequest.from(approved(drawing, "u_fay")));

		assertThatThrownBy(() -> this.db.sql("UPDATE ownership_events SET to_owner = 'u_mallory' WHERE drawing_id = ?")
			.param(drawing)
			.update()).hasMessageContaining("append-only");
		assertThatThrownBy(() -> this.db.sql("DELETE FROM ownership_events WHERE drawing_id = ?").param(drawing).update())
			.hasMessageContaining("append-only");
		assertThatThrownBy(() -> this.db.sql("INSERT INTO ownership_events (drawing_id, kind, to_owner) VALUES (?, 'MINT', 'u_x')")
			.param(drawing)
			.update()).isInstanceOf(DataAccessException.class).hasMessageContaining("ownership_events_one_mint");
		// A sale needs a seller and a sale id.
		assertThatThrownBy(() -> this.db.sql("INSERT INTO ownership_events (drawing_id, kind, to_owner) VALUES (?, 'SALE', 'u_x')")
			.param(drawing)
			.update()).hasMessageContaining("ownership_events_kind_shape");
	}

	private long eventsOf(UUID drawing) {
		return this.db.sql("SELECT COUNT(*) FROM ownership_events WHERE drawing_id = ?").param(drawing).query(Long.class).single();
	}

}
