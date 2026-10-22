package com.doodlearena.analytics;

import static org.junit.jupiter.api.Assertions.assertEquals;

import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.attribute.FileTime;
import java.util.Optional;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/** Finding the checkpoint to start from, on the local file system (the same API as S3). */
class CheckpointRestoreTest {
	@TempDir
	Path dir;

	private Path checkpoint(String job, int n, boolean complete, long time) throws Exception {
		Path chk = Files.createDirectories(dir.resolve(job).resolve("chk-" + n));
		if (complete) {
			Files.setLastModifiedTime(Files.writeString(chk.resolve("_metadata"), "m"), FileTime.fromMillis(time));
		}
		return chk;
	}

	@Test
	void picksTheNewestCompletedCheckpointOfAnyJob() throws Exception {
		Files.createDirectories(dir.resolve("job-a").resolve("shared"));
		checkpoint("job-a", 7, true, 1_000);
		checkpoint("job-b", 2, true, 2_000);
		checkpoint("job-b", 3, false, 0); // in progress when the JobManager stopped

		String found = CheckpointRestore.latest(dir.toUri().toString()).orElseThrow();
		assertEquals(dir.resolve("job-b").resolve("chk-2"), Path.of(java.net.URI.create(found)));
	}

	@Test
	void startsEmptyWithoutCheckpoints() throws Exception {
		assertEquals(Optional.empty(), CheckpointRestore.latest(dir.toUri().toString()));
		assertEquals(Optional.empty(), CheckpointRestore.latest(dir.resolve("missing").toUri().toString()));
		assertEquals(Optional.empty(), CheckpointRestore.choose("latest", ""));
	}

	@Test
	void theSettingOverridesTheSearch() throws Exception {
		checkpoint("job-a", 1, true, 1_000);
		String root = dir.toUri().toString();
		assertEquals(Optional.empty(), CheckpointRestore.choose("none", root));
		assertEquals(Optional.of("s3://b/savepoint-1"), CheckpointRestore.choose("s3://b/savepoint-1", root));
		assertEquals(CheckpointRestore.latest(root), CheckpointRestore.choose(null, root));
	}
}
