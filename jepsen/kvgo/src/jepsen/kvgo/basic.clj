(ns jepsen.kvgo.basic
  "Basic workload: concurrent reads and writes against a single register.
  No nemesis — proves the plumbing works and baseline correctness holds.
  Uses PinnedClient: each worker talks only to its assigned node."
  (:require [jepsen [checker :as checker]
                    [generator :as gen]
                    [nemesis :as nemesis]]
            [jepsen.checker.timeline :as timeline]
            [jepsen.kvgo.support :as support]
            [knossos.model :as model]))

(defn workload
  "Returns a map of :client, :generator, :checker, :nemesis for the basic test."
  [opts]
  {:client    (support/->PinnedClient nil)
   :nemesis   nemesis/noop
   :checker   (checker/compose
                {:linear   (checker/linearizable
                             {:model     (model/register nil)
                              :algorithm :wgl})
                 :perf     (checker/perf)
                 :timeline (timeline/html)})
   :generator (->> (gen/mix [support/r support/w])
                   (gen/stagger 1/5)
                   (gen/nemesis nil)
                   (gen/time-limit (:time-limit opts 30)))})
