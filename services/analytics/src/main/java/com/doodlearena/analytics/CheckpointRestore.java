package com.doodlearena.analytics;

import java.io.IOException;
import java.util.Optional;
import org.apache.flink.core.fs.FileStatus;
import org.apache.flink.core.fs.FileSystem;
import org.apache.flink.core.fs.Path;

/**
 * Where a new JobManager starts the job (ADR 0018). Flink without high availability starts every
 * submitted job empty, and wash trading's state is 7 days of sales, so the job looks for the
 * newest completed checkpoint itself and starts from it.
 *
 * <p>It lists the checkpoint directory through Flink's own FileSystem, so it reads the bucket the
 * way checkpointing writes it: the S3 plugin with the same credentials, locally and in AWS. A
 * checkpoint is complete once its {@code _metadata} file exists.
 */
final class CheckpointRestore {
	private CheckpointRestore() {}

	/**
	 * The path to start from: ANALYTICS_RESTORE=none starts empty, any other value but "latest"
	 * (the default) is a checkpoint or savepoint path to use as it is.
	 */
	static Optional<String> choose(String setting, String checkpointsDir) throws IOException {
		if (setting == null || setting.isEmpty() || setting.equals("latest")) {
			return checkpointsDir == null || checkpointsDir.isEmpty() ? Optional.empty() : latest(checkpointsDir);
		}
		return setting.equals("none") ? Optional.empty() : Optional.of(setting);
	}

	/**
	 * The newest completed checkpoint under dir, whatever job made it: dir/<job id>/chk-<n> with a
	 * {@code _metadata} file, newest by that file's time.
	 */
	static Optional<String> latest(String dir) throws IOException {
		Path root = new Path(dir);
		FileSystem fs = root.getFileSystem();
		if (!fs.exists(root)) {
			return Optional.empty();
		}
		Path best = null;
		long bestTime = Long.MIN_VALUE;
		for (FileStatus job : list(fs, root)) {
			if (!job.isDir()) {
				continue;
			}
			for (FileStatus chk : list(fs, job.getPath())) {
				if (!chk.isDir() || !chk.getPath().getName().startsWith("chk-")) {
					continue; // shared/ and taskowned/ hold state files, not checkpoints
				}
				Path metadata = new Path(chk.getPath(), "_metadata");
				if (!fs.exists(metadata)) {
					continue; // never completed
				}
				long time = fs.getFileStatus(metadata).getModificationTime();
				if (time > bestTime) {
					best = chk.getPath();
					bestTime = time;
				}
			}
		}
		return Optional.ofNullable(best).map(Path::toString);
	}

	private static FileStatus[] list(FileSystem fs, Path dir) throws IOException {
		FileStatus[] found = fs.listStatus(dir);
		return found == null ? new FileStatus[0] : found;
	}
}
