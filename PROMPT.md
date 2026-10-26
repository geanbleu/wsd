# Prompt de démarrage pour Claude Code

À coller dans Claude Code, lancé à la racine du repo qui contient `CLAUDE.md` et `docs/`.

---

Lis `CLAUDE.md`, `docs/SPEC.md` et `docs/PLAN.md` en entier.

Avant d'écrire du code :

1. Résume-moi en 10 lignes maximum ce que tu as compris de l'architecture.
2. Liste les points de la spec qui dépendent de la doc amont (API Firecracker, jailer, code-server, opencode). Pour chacun, vérifie la doc ou le swagger de la version la plus récente et dis-moi si la spec est correcte ou ce qu'il faut corriger. Propose les versions à épingler dans `deploy/versions.env`.
3. Signale toute incohérence ou ambiguïté de la spec.

Ensuite, attaque la **phase 0** du plan, puis enchaîne sur la **phase 1**. Pour chaque phase :

- crée une liste de tâches ;
- commit à chaque étape logique ;
- termine en exécutant les critères d'acceptation et en me montrant la sortie ;
- arrête-toi et attends mon feu vert avant de passer à la phase suivante.

Ne lance aucune commande qui modifie la configuration réseau ou les répertoires de `/var/lib/wsd` et `/srv/jailer` sans me le demander, sauf dans les tests d'intégration.
