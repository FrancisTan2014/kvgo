(ns jepsen.kvgo.partition
  "Partition workload: concurrent reads and writes under network partitions.
  Expects {:valid? false} — proves stale reads exist when a partitioned leader
  serves from its local state machine without ReadIndex.
  Uses BroadcastClient: reads pinned (to catch stale values), writes retry
  across all nodes (to find the new leader during partition)."
  (:require [jepsen [checker :as checker]
                    [generator :as gen]
                    [nemesis :as nemesis]]
            [jepsen.checker.timeline :as timeline]
            [jepsen.kvgo.support :as support]
            [knossos.model :as model]))

(defn workload
  "Returns a map of :client, :generator, :checker, :nemesis for the partition test."
  [opts]
  {:client    (support/->BroadcastClient nil nil)
   :nemesis   (nemesis/partition-random-halves)
   :checker   (checker/compose
                {:linear   (checker/linearizable
                             {:model     (model/register nil)
                              :algorithm :wgl})
                 :perf     (checker/perf)
                 :timeline (timeline/html)})
   :generator (->> (gen/mix [support/r support/w])
                   (gen/stagger 1/50)
                   (gen/nemesis
                     (cycle [(gen/sleep 5)
                             {:type :info, :f :start}
                             (gen/sleep 10)
                             {:type :info, :f :stop}]))
                   (gen/time-limit (:time-limit opts 60)))})
