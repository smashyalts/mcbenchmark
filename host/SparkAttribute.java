import me.lucko.spark.paper.proto.SparkSamplerProtos.SamplerData;
import me.lucko.spark.paper.proto.SparkSamplerProtos.StackTraceNode;
import me.lucko.spark.paper.proto.SparkSamplerProtos.ThreadNode;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;

/**
 * Attributes a spark profile to a package prefix, using spark's own protobuf
 * classes (shipped inside spark-paper.jar) rather than a hand-rolled decoder.
 *
 * This exists because a generic protobuf walker gets spark's format wrong in a
 * way that looks plausible. spark does NOT nest StackTraceNodes: a ThreadNode
 * holds a flat pool of nodes and each node points at its children by index
 * (childrenRefs). Walking it generically treats every node as a root, so parent
 * and child time are both counted into the total and nothing propagates down a
 * stack — the resulting percentages are wrong but not obviously so.
 *
 * Times are inclusive (a node's time covers its subtree), so "time under the
 * prefix" is the sum over the *topmost* matching nodes on each path; descending
 * further would double-count.
 *
 *   javac -cp spark-paper.jar -d out host/SparkAttribute.java
 *   java  -cp "out;spark-paper.jar" SparkAttribute <file> [prefix] [thread]
 */
public final class SparkAttribute {

    public static void main(String[] args) throws Exception {
        if (args.length < 1) {
            System.err.println("usage: SparkAttribute <payload> [prefix] [thread]");
            System.exit(2);
        }
        byte[] raw = Files.readAllBytes(Path.of(args[0]));
        String prefix = args.length > 1 ? args[1] : "com.mcbench";
        String wantThread = args.length > 2 ? args[2] : "Server thread";

        SamplerData data = SamplerData.parseFrom(raw);
        System.out.printf("payload %,d bytes, %d thread(s)%n", raw.length, data.getThreadsCount());

        boolean found = false;
        for (ThreadNode thread : data.getThreadsList()) {
            if (!wantThread.isEmpty() && !thread.getName().equals(wantThread)) {
                continue;
            }
            found = true;
            report(thread, prefix);
        }
        if (!found) {
            System.out.println("thread not found. present:");
            for (ThreadNode t : data.getThreadsList()) {
                System.out.println("   " + t.getName());
            }
        }
    }

    private static void report(ThreadNode thread, String prefix) {
        List<StackTraceNode> pool = thread.getChildrenList();

        // Any node named as someone's child is not a root.
        Set<Integer> referenced = new HashSet<>();
        for (StackTraceNode n : pool) {
            referenced.addAll(n.getChildrenRefsList());
        }
        List<Integer> roots = new ArrayList<>();
        for (int i = 0; i < pool.size(); i++) {
            if (!referenced.contains(i)) {
                roots.add(i);
            }
        }

        double sum = 0;
        for (int r : roots) {
            sum += time(pool.get(r));
        }
        final double total = sum;

        Map<String, Double> ours = new HashMap<>();
        Map<String, Double> selfByFrame = new HashMap<>();
        double[] attributed = {0};
        Set<Integer> guard = new HashSet<>();
        for (int r : roots) {
            walk(pool, r, prefix, false, ours, selfByFrame, attributed, guard);
        }

        System.out.printf("%n=== spark thread: %s ===%n", thread.getName());
        System.out.printf("nodes=%,d roots=%,d  total inclusive time=%,.0f%n",
                pool.size(), roots.size(), total);
        double pct = total > 0 ? 100.0 * attributed[0] / total : 0;
        System.out.printf(">>> time under '%s': %,.0f  (%.4f%%)%n", prefix, attributed[0], pct);
        ours.entrySet().stream()
                .sorted(Map.Entry.<String, Double>comparingByValue().reversed())
                .limit(8)
                .forEach(e -> System.out.printf("    %8.4f%%  %s%n",
                        100.0 * e.getValue() / total, e.getKey()));

        System.out.println("\ntop frames by self time:");
        selfByFrame.entrySet().stream()
                .sorted(Map.Entry.<String, Double>comparingByValue().reversed())
                .limit(15)
                .forEach(e -> System.out.printf("    %7.3f%%  %s%n",
                        100.0 * e.getValue() / total, e.getKey()));
    }

    /**
     * Walks the flattened tree. Once inside the prefix we stop adding time (the
     * topmost matching node already accounts for its whole subtree) but keep
     * descending so self-time attribution stays complete.
     */
    private static void walk(List<StackTraceNode> pool, int idx, String prefix, boolean inside,
                             Map<String, Double> ours, Map<String, Double> selfByFrame,
                             double[] attributed, Set<Integer> guard) {
        if (!guard.add(idx)) {
            return; // defensive: refs should form a tree, but do not loop if not
        }
        StackTraceNode n = pool.get(idx);
        String cls = n.getClassName();
        boolean isOurs = cls != null && cls.startsWith(prefix);

        if (isOurs && !inside) {
            double t = time(n);
            attributed[0] += t;
            ours.merge(cls + "." + n.getMethodName(), t, Double::sum);
        }

        double childTime = 0;
        for (int c : n.getChildrenRefsList()) {
            if (c >= 0 && c < pool.size()) {
                childTime += time(pool.get(c));
                walk(pool, c, prefix, inside || isOurs, ours, selfByFrame, attributed, guard);
            }
        }
        double self = Math.max(0, time(n) - childTime);
        if (self > 0) {
            selfByFrame.merge(cls + "." + n.getMethodName(), self, Double::sum);
        }
        guard.remove(idx);
    }

    /** A node's time, summed across spark's time windows. */
    private static double time(StackTraceNode n) {
        double t = 0;
        for (int i = 0; i < n.getTimesCount(); i++) {
            t += n.getTimes(i);
        }
        return t;
    }
}
