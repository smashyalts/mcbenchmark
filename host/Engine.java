/**
 * Engine prints which sampler produced a spark profile.
 *
 * This matters before quoting any percentage from a profile. spark uses
 * async-profiler where it can and falls back to its own Java sampler otherwise
 * (notably on Windows). The Java sampler is safepoint-biased: it samples only at
 * safepoint-pollable locations and cannot see native frames, so it
 * over-attributes Java packages — measured at roughly 5x on the same workload.
 * Same ranking, wrong magnitudes.
 *
 *   javac -cp spark-paper.jar -d out host/Engine.java
 *   java  -cp "out;spark-paper.jar" Engine <payload>
 */
import me.lucko.spark.paper.proto.SparkSamplerProtos.SamplerData;
import java.nio.file.*;
public class Engine {
  public static void main(String[] a) throws Exception {
    SamplerData d = SamplerData.parseFrom(Files.readAllBytes(Path.of(a[0])));
    var m = d.getMetadata();
    System.out.println("engine=" + m.getSamplerEngine()
        + " mode=" + m.getSamplerMode()
        + " interval=" + m.getInterval()
        + " os=" + m.getSystemStatistics().getOs().getName());
  }
}
